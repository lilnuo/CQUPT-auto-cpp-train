package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	_ "github.com/go-sql-driver/mysql" // 注册 "mysql" driver，只用到它的 init 副作用

	"cqupt/train"
)

// ============================================================================
// store.go —— 数据访问层
//
// 这一层只认识数据库，不认识 HTTP，也不认识浏览器。好处是：
//   - 换存储（比如把 MySQL 换成 PostgreSQL）只改这一个文件；
//   - 队列语义（抢任务、回收、状态流转）可以脱离 HTTP 单独测。
// 依赖方向是单向的：api → store，worker → store，store → 数据库。
// 反过来 store 调用 api 就是循环依赖，那说明职责分错了。
// ============================================================================

// TaskStatus 是任务状态机的取值。
//
// 状态集合刻意做得小。每多一个状态，就要多想清楚"谁能从它转到谁"，
// 而漏掉一条转移路径的后果，往往是任务永远卡在某个状态没人处理。
type TaskStatus string

const (
	StatusPending   TaskStatus = "pending"   // 排队中，可被领取
	StatusRunning   TaskStatus = "running"   // 已被某个 worker 领走
	StatusSucceeded TaskStatus = "succeeded" // 跑完且成功
	StatusFailed    TaskStatus = "failed"    // 跑完但失败
	StatusCanceled  TaskStatus = "canceled"  // 人工取消
)

// Terminal 报告该状态是否是终态（不会再变）。
func (s TaskStatus) Terminal() bool {
	return s == StatusSucceeded || s == StatusFailed || s == StatusCanceled
}

// Task 是一个刷题任务。
type Task struct {
	ID           int64
	Username     string
	Mode         string
	Num          int
	Status       TaskStatus
	Error        string
	Score        *int // 用指针区分"0 分"和"还没跑"
	QuestionDone int
	TryCount     int
	MaxTry       int
	CreatedAt    time.Time
	StartedAt    *time.Time
	FinishedAt   *time.Time

	// PasswordEnc 只在 worker 内部使用，不参与 JSON 序列化。
	// 结构体字段名首字母大写不代表必须对外暴露——是否暴露由 api 层决定，
	// api 层用的是专门的响应结构体（见 api.go 的 taskView）。
	PasswordEnc []byte
}

// AnswerRow 是某道题的作答结果。
type AnswerRow struct {
	Pid      string
	Question string
	Answer   string
	Score    float64
	Tries    int
	Passed   bool
}

// EventRow 是一条进度事件。
type EventRow struct {
	Seq    int
	Kind   string
	Detail string
	Pid    string
	Score  float64
	At     time.Time
}

// Store 封装数据库访问。
type Store struct {
	db  *sql.DB
	log *slog.Logger
}

// OpenStore 建立连接池并探活。
func OpenStore(dsn string, log *slog.Logger) (*Store, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}

	// 连接池参数。刷题服务的并发度天然很低（worker 是串行的），
	// 但 HTTP 侧读进度可能并发，所以留一点余量。
	// 不设 ConnMaxLifetime 的话，MySQL 默认 8 小时会掐掉空闲连接，
	// 而连接池不知情，下次拿到就是 "invalid connection" —— 所以必须设得比它短。
	db.SetMaxOpenConns(16)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(30 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("连不上数据库（请先执行 server/schema.sql 并检查 DB_* 环境变量）: %w", err)
	}
	return &Store{db: db, log: log}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// DB 暴露底层连接池，仅供测试构造场景使用。
func (s *Store) DB() *sql.DB { return s.db }

// ---------------------------------------------------------------------------
// 任务：写入与读取
// ---------------------------------------------------------------------------

// CreateTask 插入一个新任务，返回任务 id。
//
// 注意 passwordEnc 是**密文**。这个函数拿不到明文——加密动作发生在
// api 层，明文在那里的生命周期只有一个函数调用那么长。
func (s *Store) CreateTask(ctx context.Context, username, mode string, num int, passwordEnc []byte, maxTry int) (int64, error) {
	const q = `INSERT INTO tasks (username, mode, num, password_enc, max_try, status)
	           VALUES (?, ?, ?, ?, ?, 'pending')`
	res, err := s.db.ExecContext(ctx, q, username, mode, num, passwordEnc, maxTry)
	if err != nil {
		return 0, fmt.Errorf("创建任务失败: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("取任务 id 失败: %w", err)
	}
	return id, nil
}

const taskColumns = `id, username, mode, num, status, COALESCE(error,''), score,
                     question_done, try_count, max_try, created_at, started_at, finished_at, password_enc`

func scanTask(row interface{ Scan(...any) error }) (*Task, error) {
	var t Task
	var status string
	err := row.Scan(&t.ID, &t.Username, &t.Mode, &t.Num, &status, &t.Error, &t.Score,
		&t.QuestionDone, &t.TryCount, &t.MaxTry, &t.CreatedAt, &t.StartedAt, &t.FinishedAt, &t.PasswordEnc)
	if err != nil {
		return nil, err
	}
	t.Status = TaskStatus(status)
	return &t, nil
}

// ErrTaskNotFound 表示任务不存在。
var ErrTaskNotFound = errors.New("任务不存在")

// GetTask 按 id 取任务。
func (s *Store) GetTask(ctx context.Context, id int64) (*Task, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id = ?`, id)
	t, err := scanTask(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrTaskNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("查询任务失败: %w", err)
	}
	return t, nil
}

// ListTasks 按创建时间倒序列出任务，用于进度页。
// limit 由调用方限制，避免一次把整张表拉出来。
func (s *Store) ListTasks(ctx context.Context, limit int) ([]*Task, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+taskColumns+` FROM tasks ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("列出任务失败: %w", err)
	}
	defer rows.Close()

	var out []*Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, fmt.Errorf("扫描任务失败: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// StatusCount 返回各状态的任务数，用于进度页顶部的概览。
func (s *Store) StatusCount(ctx context.Context) (map[TaskStatus]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT status, COUNT(*) FROM tasks GROUP BY status`)
	if err != nil {
		return nil, fmt.Errorf("统计任务状态失败: %w", err)
	}
	defer rows.Close()

	out := map[TaskStatus]int{}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[TaskStatus(st)] = n
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// 队列：领任务、续租、回收、收尾
//
// 这四条 SQL 就是"队列"的全部。没有额外的中间件，因为 tasks 这一行
// 既描述业务（谁要刷几道题），也描述排队状态（该不该被领）。两者同生共死，
// 天然不存在"任务写进去了但消息没发出去"这类双写不一致。
// ---------------------------------------------------------------------------

// claimSQL 是抢任务的完整语句。
//
// 提成包级常量的唯一目的是让测试能对**同一条语句**做 EXPLAIN：
// 如果测试里再抄一份 SQL，两边一旦不同步，测的就是一句谁也没在用的语句，
// 而真正的查询可能已经悄悄退化成全表扫描了。
const claimSQL = `SELECT id FROM tasks FORCE INDEX (idx_claim)
                  WHERE status = 'pending' AND available_at <= NOW(3)
                  ORDER BY id
                  LIMIT 1
                  FOR UPDATE SKIP LOCKED`

// ClaimTask 尝试领取一个待执行任务。
//
// 返回 (nil, nil) 表示当前没有可领的任务——这不是错误，是队列空，
// 调用方应当睡一会儿再来。把这个情况做成 error 会诱导调用方去记日志，
// 而队列空是常态，不该刷日志。
func (s *Store) ClaimTask(ctx context.Context, workerID string) (*Task, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("开启事务失败: %w", err)
	}
	// 用 defer 兜底回滚。如果下面任何一步 return 了，事务一定被释放，
	// 不会因为漏写 rollback 而把行锁攥在手里，把整个队列卡死。
	defer func() { _ = tx.Rollback() }()

	// 这一句是整个队列的核心。
	//
	// FOR UPDATE      给选中的行加排他锁，防止两个 worker 同时改同一行；
	// SKIP LOCKED     遇到已被锁住的行**直接跳过**，而不是排队等锁。
	//
	// 为什么必须有 SKIP LOCKED：没有它的话，10 个 worker 同时来抢，
	// 只有 1 个能拿到锁，另外 9 个全在原地等——它们等的那一行马上就会被
	// 改成 running，等到了也是白等。结果是吞吐量被锁等待拖垮，
	// 而且 worker 数越多越慢。SKIP LOCKED 让空手而归的 worker 立刻去试下一行，
	// 这才是"多消费者"该有的样子。
	//
	// ORDER BY id 配 idx_claim (status, id)：status 等值定位到一段连续区间，
	// 区间内就是 id 升序，所以排序完全由索引满足，没有 filesort。
	//
	// FORCE INDEX 不是随手加的，它治的是一个实测出来的退化：
	// 队列空、表里积着历史任务时，优化器会改用主键全表扫描（5000 行读了全部、
	// 4.6ms），而空队列恰恰是轮询最频繁的时刻。强制走 idx_claim 后是 0.047ms，
	// 且不随表增长。详细实测数据见 schema.sql 里索引 1 的注释。
	var id int64
	switch err := tx.QueryRowContext(ctx, claimSQL).Scan(&id); {
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil // 队列空
	case err != nil:
		return nil, fmt.Errorf("选任务失败: %w", err)
	}

	// 领走：改状态、记下谁领的、领取时刻，并把已尝试次数 +1。
	// try_count 在这里自增而不是在 worker 里自增，是为了让"领取"这个动作
	// 原子地留下痕迹——哪怕 worker 起来就崩了，这次尝试也算数，
	// 否则一个必然崩溃的任务会被无限重领。
	const take = `UPDATE tasks
	              SET status = 'running',
	                  locked_by = ?,
	                  locked_at = NOW(3),
	                  started_at = COALESCE(started_at, NOW(3)),
	                  try_count = try_count + 1
	              WHERE id = ?`
	if _, err := tx.ExecContext(ctx, take, workerID, id); err != nil {
		return nil, fmt.Errorf("领取任务失败: %w", err)
	}

	t, err := scanTask(tx.QueryRowContext(ctx, `SELECT `+taskColumns+` FROM tasks WHERE id = ?`, id))
	if err != nil {
		return nil, fmt.Errorf("读取已领取任务失败: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("提交事务失败: %w", err)
	}
	return t, nil
}

// TouchLock 续租：把 locked_at 推到当前时刻。
//
// 为什么要续租：RecoverStale 靠"locked_at 太久没动"来判断任务僵死。
// 而一个正常的任务可能跑十几分钟，期间 locked_at 一直不变，就会被误判、
// 被另一个 worker 抢走重跑——同一个学号被两个浏览器同时登录，
// 前面的会话会被踢掉，两边都乱套。
// worker 因此在执行期间定时调用本方法，相当于告诉别人"我还活着"。
func (s *Store) TouchLock(ctx context.Context, taskID int64, workerID string) error {
	const q = `UPDATE tasks SET locked_at = NOW(3)
	           WHERE id = ? AND locked_by = ? AND status = 'running'`
	res, err := s.db.ExecContext(ctx, q, taskID, workerID)
	if err != nil {
		return fmt.Errorf("续租失败: %w", err)
	}
	// 影响行数为 0 有两种截然不同的含义，必须先分清，不能直接下结论：
	//
	//	1. 这行已经不属于我们了（被回收、被取消、或已结束）→ 真的丢锁；
	//	2. 锁还在我们手里，只是**新值与旧值相同**，MySQL 认为没有变化。
	//
	// 第 2 种是本项目实测踩到过的坑：RowsAffected 返回的是**实际改变的行数**，
	// 不是匹配的行数。locked_at 是 DATETIME(3)，只要这次续租和上次落在同一毫秒内，
	// 新的 NOW(3) 与旧的完全相等，MySQL 就报 0 行。实测（固定会话时间）：
	//
	//	第 1 次写入 NOW(3)          → Rows matched: 1  Changed: 1
	//	第 2 次写入同一个 NOW(3)    → Rows matched: 1  Changed: 0   ★
	//	第 3 次换成不同时刻          → Rows matched: 1  Changed: 1
	//
	// 旧代码把 0 直接当丢锁，后果是 worker 会主动中止一个完全健康的任务——
	// 一个**静默的错误答案**，比报错难查得多。
	//
	// 心跳间隔是 LOCK_TIMEOUT/3、下限 10 秒，正常配置下撞进同一毫秒几乎不可能，
	// 所以线上极难触发；但把 LOCK_TIMEOUT 配得很小就会，而测试里每轮都撞。
	// 与其依赖"间隔够大"这个隐含前提，不如把 0 行当成一个需要复核的信号。
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("读取续租结果失败: %w", err)
	} else if n > 0 {
		return nil
	}

	// 只有 0 行才走这里：复核一次，看锁到底在谁手里。
	// 这条 SELECT 只在"可能丢锁"时才执行，正常心跳路径上一次都不会有。
	var owner, status string
	err = s.db.QueryRowContext(ctx,
		`SELECT COALESCE(locked_by, ''), status FROM tasks WHERE id = ?`, taskID).Scan(&owner, &status)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ErrLockLost // 任务行都没了，锁自然不在
	case err != nil:
		return fmt.Errorf("复核任务锁失败: %w", err)
	}
	if owner == workerID && status == string(StatusRunning) {
		// 锁确实是我们的，只是这一刻时间没往前走。什么都不用改——
		// locked_at 已经是"刚刚"，本来就不算旧。
		return nil
	}
	return ErrLockLost
}

// ErrLockLost 表示任务的锁已经不在自己手上。
var ErrLockLost = errors.New("任务锁已丢失（可能被回收或取消）")

// RecoverStale 把僵死任务放回队列。
//
// 什么算僵死：状态还是 running，但 locked_at 已经超过 timeout 没更新过。
// 典型原因是 worker 进程被 kill -9——它没机会把任务标成失败，
// 那一行就永远停在 running，谁也不会再碰它。没有这个补偿，
// 队列会随着每次异常退出慢慢"漏"掉任务，而且完全静默。
//
// 注意 available_at 设成当前时刻：刚被回收的任务立刻可领，
// 但 try_count 已经涨过了，超出 max_try 的任务会在下面被标成失败。
func (s *Store) RecoverStale(ctx context.Context, timeout time.Duration) (int64, error) {
	const q = `UPDATE tasks
	           SET status = 'pending', locked_by = NULL, locked_at = NULL, available_at = NOW(3)
	           WHERE status = 'running' AND locked_at < NOW(3) - INTERVAL ? SECOND`
	res, err := s.db.ExecContext(ctx, q, int(timeout.Seconds()))
	if err != nil {
		return 0, fmt.Errorf("回收僵死任务失败: %w", err)
	}
	return res.RowsAffected()
}

// FinishTask 收尾：写终态、总分、错误信息、完成时刻。
//
// 只在"这行还归我"时才写（locked_by 校验），避免一个已被回收的任务
// 被原来的 worker 回头覆盖成成功——那会把另一个 worker 的结果冲掉。
func (s *Store) FinishTask(ctx context.Context, taskID int64, workerID string, status TaskStatus, score *int, errMsg string) error {
	if !status.Terminal() {
		return fmt.Errorf("FinishTask 只接受终态，收到 %q", status)
	}
	const q = `UPDATE tasks
	           SET status = ?, score = ?, error = ?, finished_at = NOW(3),
	               locked_by = NULL, locked_at = NULL
	           WHERE id = ? AND locked_by = ?`
	res, err := s.db.ExecContext(ctx, q, status, score, nullIfEmpty(errMsg), taskID, workerID)
	if err != nil {
		return fmt.Errorf("收尾失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrLockLost
	}
	return nil
}

// ReleaseForRetry 把任务退回队列，等 available_at 到了再被领。
// 用于"这次没跑成，但还值得再试一次"的情况。
func (s *Store) ReleaseForRetry(ctx context.Context, taskID int64, workerID string, delay time.Duration, errMsg string) error {
	const q = `UPDATE tasks
	           SET status = 'pending', locked_by = NULL, locked_at = NULL,
	               available_at = NOW(3) + INTERVAL ? SECOND, error = ?
	           WHERE id = ? AND locked_by = ?`
	res, err := s.db.ExecContext(ctx, q, int(delay.Seconds()), nullIfEmpty(errMsg), taskID, workerID)
	if err != nil {
		return fmt.Errorf("退回队列失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrLockLost
	}
	return nil
}

// FailExhausted 把重试次数已用尽的排队任务标记为失败。
//
// 为什么需要它：RecoverStale 把僵死任务放回 pending 之后，那些已经用光重试
// 配额的任务会重新排队，然后被 worker 领走、再失败一次、再放回去——
// 无限循环。这个函数在回收之后跑一遍，把它们截住。
//
// 注意它只处理 pending：一个任务刚被 RecoverStale 退回 pending 时 try_count
// 已经等于 max_try 了，所以这里一定会命中。反过来，绝不能在任务还在 running
// 时判它耗尽——那会把正在正常执行的任务标成失败。
func (s *Store) FailExhausted(ctx context.Context) (int64, error) {
	const q = `UPDATE tasks
	           SET status = 'failed',
	               error = '重试次数已用尽（每次失败都会重试，仍未能完成）',
	               finished_at = NOW(3)
	           WHERE status = 'pending' AND try_count >= max_try`
	res, err := s.db.ExecContext(ctx, q)
	if err != nil {
		return 0, fmt.Errorf("标记重试耗尽任务失败: %w", err)
	}
	return res.RowsAffected()
}

// CancelTask 人工取消一个还没结束的任务。
//
// 只能取消 pending 的：running 的那个正在浏览器里跑，直接改状态会让
// worker 收尾时撞上 ErrLockLost，反而看不清到底发生了什么。
// 想停 running 的任务，正确做法是等它超时或让 worker 检查取消标记。
func (s *Store) CancelTask(ctx context.Context, taskID int64) error {
	const q = `UPDATE tasks SET status = 'canceled', finished_at = NOW(3)
	           WHERE id = ? AND status = 'pending'`
	res, err := s.db.ExecContext(ctx, q, taskID)
	if err != nil {
		return fmt.Errorf("取消任务失败: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errors.New("只有排队中的任务可以取消（正在执行或已结束的任务改不动）")
	}
	return nil
}

// ---------------------------------------------------------------------------
// 进度事件
// ---------------------------------------------------------------------------

// MaxEventSeq 返回某任务已有事件的最大 seq，没有事件时返回 0。
//
// 为什么不让数据库自增 seq：event 的 id 已经是自增主键了，但 id 是全局递增的，
// 拿它做"同一任务内的第几条"会让进度页上的序号跳着走（1、7、13…）。
// 而且断了重连之后要接着编号，读一次 MAX 比在内存里维护计数器更可靠。
func (s *Store) MaxEventSeq(ctx context.Context, taskID int64) (int, error) {
	var seq sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT MAX(seq) FROM task_events WHERE task_id = ?`, taskID).Scan(&seq)
	if err != nil {
		return 0, fmt.Errorf("查询事件序号失败: %w", err)
	}
	return int(seq.Int64), nil
}

// AppendEvent 追加一条进度事件。
//
// 用 INSERT ... ON DUPLICATE KEY UPDATE 而不是裸 INSERT：
// (task_id, seq) 上有唯一键，重复上报同一个序号时把它变成幂等更新，
// 而不是抛错。事件上报失败不该让整个任务崩掉——它是"锦上添花"的数据。
func (s *Store) AppendEvent(ctx context.Context, taskID int64, seq int, e train.Event) error {
	const q = `INSERT INTO task_events (task_id, seq, kind, detail, pid, score)
	           VALUES (?, ?, ?, ?, ?, ?)
	           ON DUPLICATE KEY UPDATE kind = VALUES(kind), detail = VALUES(detail),
	                                   pid = VALUES(pid), score = VALUES(score)`
	if _, err := s.db.ExecContext(ctx, q,
		taskID, seq, e.Kind, truncateRunes(e.Detail, 500), e.Pid, e.Score); err != nil {
		return fmt.Errorf("写入事件失败: %w", err)
	}
	return nil
}

// ListEvents 按序号升序取事件，用于展示任务的时间线。
func (s *Store) ListEvents(ctx context.Context, taskID int64) ([]EventRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT seq, kind, detail, pid, score, created_at
		 FROM task_events WHERE task_id = ? ORDER BY seq`, taskID)
	if err != nil {
		return nil, fmt.Errorf("查询事件失败: %w", err)
	}
	defer rows.Close()

	var out []EventRow
	for rows.Next() {
		var e EventRow
		if err := rows.Scan(&e.Seq, &e.Kind, &e.Detail, &e.Pid, &e.Score, &e.At); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------------------
// 每题结果
// ---------------------------------------------------------------------------

// UpsertResult 写入/更新某道题的结果。
//
// 重答会让同一道题被写多次，所以必须是 upsert 而不是 insert。
// 依赖 (task_id, pid) 上的唯一键：即便应用层判断失误重复调用，
// 数据库这一层也能保证"一道题一行"，不会长出重复记录。
func (s *Store) UpsertResult(ctx context.Context, taskID int64, r AnswerRow) error {
	const q = `INSERT INTO task_results (task_id, pid, question, answer, score, tries, passed)
	           VALUES (?, ?, ?, ?, ?, ?, ?)
	           ON DUPLICATE KEY UPDATE
	               question = VALUES(question), answer = VALUES(answer),
	               score = VALUES(score), tries = VALUES(tries), passed = VALUES(passed)`
	if _, err := s.db.ExecContext(ctx, q, taskID, r.Pid,
		truncateRunes(r.Question, 8000), truncateRunes(r.Answer, 4000),
		r.Score, r.Tries, r.Passed); err != nil {
		return fmt.Errorf("写入题目结果失败: %w", err)
	}
	return nil
}

// ListResults 取某任务的全部题目结果。
func (s *Store) ListResults(ctx context.Context, taskID int64) ([]AnswerRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT pid, COALESCE(question,''), COALESCE(answer,''), score, tries, passed
		 FROM task_results WHERE task_id = ? ORDER BY id`, taskID)
	if err != nil {
		return nil, fmt.Errorf("查询题目结果失败: %w", err)
	}
	defer rows.Close()

	var out []AnswerRow
	for rows.Next() {
		var r AnswerRow
		if err := rows.Scan(&r.Pid, &r.Question, &r.Answer, &r.Score, &r.Tries, &r.Passed); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// BumpQuestionDone 把已完成题数 +1。
func (s *Store) BumpQuestionDone(ctx context.Context, taskID int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE tasks SET question_done = question_done + 1 WHERE id = ?`, taskID)
	if err != nil {
		return fmt.Errorf("更新进度失败: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

// nullIfEmpty 把空串转成 NULL。
//
// 空串和 NULL 在 SQL 里语义不同：error 列为 NULL 表示"没出错"，
// 空串则是个含糊的"出错了但没说什么"。用 NULL 更准确，
// 也让 WHERE error IS NOT NULL 这类查询能正常工作。
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// truncateRunes 按字符（而非字节）截断，避免把中文切成乱码。
//
// 这是 v1.2.0 里踩过的坑：s[:n] 是按字节切的，一个中文字 3 字节，
// 切在中间就会得到半个字符，写进数据库就是问号或乱码。
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
