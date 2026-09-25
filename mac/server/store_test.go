package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"cqupt/train"
)

// ============================================================================
// store_test.go —— 服务端测试
//
// 分成两拨：
//
//   1. 纯逻辑测试（不需要任何外部依赖）：加密、配置校验、错误分类、
//      模板转义、中间件。这些是默认就跑的。
//   2. 数据库测试（需要 MySQL）：队列语义是这一版的核心，而"并发领任务
//      不会重复领取"这种事只有真数据库才验得出来——用假的 Store 去测，
//      测的是假实现有没有照着我的假设走，不是 MySQL 会不会那么做。
//      所以这拨测试连真库，默认跳过，设了 CQUPT_TEST_DSN 就跑。
//
//      本机执行：
//        CQUPT_TEST_DSN='cqupt_app:cqupt_app_dev@tcp(127.0.0.1:3306)/cqupt_train?parseTime=true&loc=Local&charset=utf8mb4' \
//          go test ./server/ -v
// ============================================================================

// ---------------------------------------------------------------------------
// 数据库测试脚手架
// ---------------------------------------------------------------------------

const testDSNEnv = "CQUPT_TEST_DSN"

// testStore 返回一个连上测试库的 Store；没配 DSN 就跳过。
func testStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv(testDSNEnv)
	if dsn == "" {
		t.Skipf("未设置 %s，跳过需要数据库的测试（用法见本文件顶部注释）", testDSNEnv)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := OpenStore(dsn, log)
	if err != nil {
		t.Fatalf("连不上测试数据库: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// taskPrefix 返回本次测试独有的学号前缀。
//
// 为什么不用"截断整张表"来隔离：测试库和开发库是同一个，
// 一把清空会把开发时手动塞进去、正等着观察的任务也删掉。
// 给每次测试一个独有前缀，只清自己那几行。
func taskPrefix(t *testing.T) string {
	return fmt.Sprintf("zztest%d", time.Now().UnixNano()%1_000_000_000)
}

// cleanupTasks 删掉本前缀产生的任务及其子表数据。
//
// 必须先删子表：schema 里没有建外键（有意为之，避免删除任务时被约束绊住），
// 所以级联删除得自己来。
func cleanupTasks(t *testing.T, s *Store, prefix string) {
	t.Helper()
	ctx := context.Background()
	like := prefix + "%"
	for _, q := range []string{
		`DELETE FROM task_events  WHERE task_id IN (SELECT id FROM (SELECT id FROM tasks WHERE username LIKE ?) x)`,
		`DELETE FROM task_results WHERE task_id IN (SELECT id FROM (SELECT id FROM tasks WHERE username LIKE ?) x)`,
		`DELETE FROM tasks WHERE username LIKE ?`,
	} {
		if _, err := s.DB().ExecContext(ctx, q, like); err != nil {
			t.Logf("清理测试数据失败（不影响结论）: %v", err)
		}
	}
}

// mustCreateTask 建一个任务，测试结束时自动清理。
func mustCreateTask(t *testing.T, s *Store, username string, maxTry int) int64 {
	t.Helper()
	// 密码列 NOT NULL，所以给一段密文。测试里不需要它解得出原文。
	enc, err := EncryptPassword(strings.Repeat("k", 32), "pw-"+username)
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	id, err := s.CreateTask(context.Background(), username, train.ModeQuiz, 3, enc, maxTry)
	if err != nil {
		t.Fatalf("创建任务失败: %v", err)
	}
	return id
}

// ---------------------------------------------------------------------------
// 队列：这一版的核心
// ---------------------------------------------------------------------------

// TestClaimTaskExactlyOnceUnderConcurrency 是本版最重要的一条测试。
//
// 它验的是：多个 worker 并发抢任务时，**同一个任务只会被领走一次**。
// 这条性质靠的是 SELECT ... FOR UPDATE SKIP LOCKED：
//   - FOR UPDATE  让"选出来"和"改成 running"之间别人插不进来；
//   - SKIP LOCKED 让抢不到的 worker 立刻转去抢下一行，而不是排队等锁。
//
// 去掉 SKIP LOCKED，这条测试依然可能通过（只是慢）；去掉 FOR UPDATE，
// 它就会稳定失败——两个 worker 会同时选中同一行。
func TestClaimTaskExactlyOnceUnderConcurrency(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	prefix := taskPrefix(t)
	t.Cleanup(func() { cleanupTasks(t, s, prefix) })

	const taskCount = 12
	mine := make(map[int64]bool, taskCount)
	for i := 0; i < taskCount; i++ {
		mine[mustCreateTask(t, s, fmt.Sprintf("%s-%02d", prefix, i), 1)] = true
	}

	const workerCount = 16
	var mu sync.Mutex
	claimedBy := map[int64][]string{}
	var wg sync.WaitGroup
	start := make(chan struct{})

	for w := 0; w < workerCount; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			workerID := fmt.Sprintf("w%d", w)
			<-start // 一起出发，才叫并发
			for {
				got, err := s.ClaimTask(ctx, workerID)
				if err != nil {
					t.Errorf("%s 领任务出错: %v", workerID, err)
					return
				}
				if got == nil {
					return // 队列已空
				}
				mu.Lock()
				claimedBy[got.ID] = append(claimedBy[got.ID], workerID)
				mu.Unlock()

				// 领到就立刻收尾：本测试要验的是"领取"的互斥性，
				// 不需要让任务一直占着 running。
				if err := s.FinishTask(ctx, got.ID, workerID, StatusSucceeded, nil, ""); err != nil {
					t.Errorf("%s 收尾任务 %d 出错: %v", workerID, got.ID, err)
					return
				}
			}
		}(w)
	}
	close(start)
	wg.Wait()

	for id := range mine {
		by := claimedBy[id]
		if len(by) != 1 {
			t.Errorf("任务 %d 被领取 %d 次（期望恰好 1 次），领取者=%v", id, len(by), by)
		}
	}
}

// TestClaimRespectsAvailableAt 验的是"没到点的任务不会被领走"。
// 这一条撑着整套重试退避机制：ReleaseForRetry 就是靠把 available_at 往后挪，
// 让失败的任务在原地等一会儿，而不是立刻又被抢去重试。
func TestClaimRespectsAvailableAt(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	prefix := taskPrefix(t)
	t.Cleanup(func() { cleanupTasks(t, s, prefix) })

	id := mustCreateTask(t, s, prefix+"-later", 3)
	// 把可用时间推到 1 小时后
	if _, err := s.DB().ExecContext(ctx,
		`UPDATE tasks SET available_at = NOW(3) + INTERVAL 3600 SECOND WHERE id = ?`, id); err != nil {
		t.Fatalf("调整 available_at 失败: %v", err)
	}

	got, err := s.ClaimTask(ctx, "w-test")
	if err != nil {
		t.Fatalf("领任务出错: %v", err)
	}
	if got != nil && got.ID == id {
		t.Errorf("available_at 在未来（1 小时后）的任务不该被领走，却领到了 %d", id)
	}

	// 改成"已经到了"，就应当能领到
	if _, err := s.DB().ExecContext(ctx,
		`UPDATE tasks SET available_at = NOW(3) - INTERVAL 1 SECOND WHERE id = ?`, id); err != nil {
		t.Fatalf("调整 available_at 失败: %v", err)
	}
	got, err = s.ClaimTask(ctx, "w-test")
	if err != nil {
		t.Fatalf("领任务出错: %v", err)
	}
	if got == nil || got.ID != id {
		t.Fatalf("available_at 已到时应当能领到任务 %d，实际 got=%v", id, got)
	}
	if got.Status != StatusRunning {
		t.Errorf("领取后状态应为 %s，实际 %s", StatusRunning, got.Status)
	}
	if got.TryCount != 1 {
		t.Errorf("领取一次后 try_count 应为 1，实际 %d", got.TryCount)
	}
	if got.StartedAt == nil {
		t.Error("领取后 started_at 应当被填上（它是「这个任务真正开始过」的凭据）")
	}
}

// TestReleaseForRetryPushesAvailableAt 验退避：退回去的任务不能被立刻再领。
func TestReleaseForRetryPushesAvailableAt(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	prefix := taskPrefix(t)
	t.Cleanup(func() { cleanupTasks(t, s, prefix) })

	id := mustCreateTask(t, s, prefix+"-retry", 3)
	if _, err := s.ClaimTask(ctx, "w1"); err != nil {
		t.Fatalf("领任务出错: %v", err)
	}
	if err := s.ReleaseForRetry(ctx, id, "w1", 30*time.Second, "验证码识别失败"); err != nil {
		t.Fatalf("退回队列出错: %v", err)
	}

	task, err := s.GetTask(ctx, id)
	if err != nil {
		t.Fatalf("查询任务出错: %v", err)
	}
	if task.Status != StatusPending {
		t.Errorf("退回后状态应为 pending，实际 %s", task.Status)
	}
	if task.Error != "验证码识别失败" {
		t.Errorf("退回时应当留下失败原因，实际 error=%q", task.Error)
	}

	got, err := s.ClaimTask(ctx, "w2")
	if err != nil {
		t.Fatalf("领任务出错: %v", err)
	}
	if got != nil && got.ID == id {
		t.Error("退避期内的任务不该被立刻领走（那退避就白做了）")
	}
}

// TestFinishTaskChecksOwnership 验"不是自己的任务写不了终态"。
//
// 为什么必须有这个校验：一个任务被回收后又派给了别人，原来那个 worker
// 如果还能写终态，就会把新 worker 的结果覆盖成自己那份——两份结果互相
// 覆盖，最后库里是谁的完全看运气。
func TestFinishTaskChecksOwnership(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	prefix := taskPrefix(t)
	t.Cleanup(func() { cleanupTasks(t, s, prefix) })

	id := mustCreateTask(t, s, prefix+"-owner", 3)
	if _, err := s.ClaimTask(ctx, "w-真主人"); err != nil {
		t.Fatalf("领任务出错: %v", err)
	}

	score := 90
	err := s.FinishTask(ctx, id, "w-冒充者", StatusSucceeded, &score, "")
	if !errors.Is(err, ErrLockLost) {
		t.Fatalf("非持有者收尾应当返回 ErrLockLost，实际 %v", err)
	}
	task, _ := s.GetTask(ctx, id)
	if task.Status != StatusRunning {
		t.Errorf("冒充者不该改动状态，实际变成了 %s", task.Status)
	}

	// 真主人可以收尾
	if err := s.FinishTask(ctx, id, "w-真主人", StatusSucceeded, &score, ""); err != nil {
		t.Fatalf("持有者收尾应当成功，实际 %v", err)
	}
	task, _ = s.GetTask(ctx, id)
	if task.Status != StatusSucceeded || task.Score == nil || *task.Score != 90 {
		t.Errorf("收尾后应为 succeeded/90，实际 %s/%v", task.Status, task.Score)
	}
	if task.FinishedAt == nil {
		t.Error("收尾后 finished_at 应当被填上")
	}
	if task.TryCount != 1 {
		t.Errorf("try_count 应保持 1（收尾不该再计数），实际 %d", task.TryCount)
	}
}

// TestFinishTaskRejectsNonTerminal 验状态机的入口约束：
// 只能写终态。允许写 running/pending 的话，状态机会退化成一个随便赋值的字段。
func TestFinishTaskRejectsNonTerminal(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for _, st := range []TaskStatus{StatusRunning, StatusPending} {
		if err := s.FinishTask(ctx, 1, "w", st, nil, ""); err == nil {
			t.Errorf("FinishTask 应当拒绝非终态 %q", st)
		} else if !strings.Contains(err.Error(), "终态") {
			t.Errorf("错误信息应说明只接受终态，实际 %q", err.Error())
		}
	}
}

// TestRecoverStaleBringsBackKilledTask 模拟 worker 被 kill -9：
// 任务停在 running，谁也不会再碰它，直到锁超时被回收。
func TestRecoverStaleBringsBackKilledTask(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	prefix := taskPrefix(t)
	t.Cleanup(func() { cleanupTasks(t, s, prefix) })

	id := mustCreateTask(t, s, prefix+"-killed", 3)
	if _, err := s.ClaimTask(ctx, "w-被kill的"); err != nil {
		t.Fatalf("领任务出错: %v", err)
	}

	// 把 locked_at 伪造成 1 小时前 —— 等价于"这个 worker 已经一小时没动静了"
	if _, err := s.DB().ExecContext(ctx,
		`UPDATE tasks SET locked_at = NOW(3) - INTERVAL 3600 SECOND WHERE id = ?`, id); err != nil {
		t.Fatalf("伪造 locked_at 失败: %v", err)
	}

	freed, err := s.RecoverStale(ctx, 20*time.Minute)
	if err != nil {
		t.Fatalf("回收僵死任务出错: %v", err)
	}
	if freed < 1 {
		t.Fatalf("应当至少回收 1 个僵死任务，实际 %d", freed)
	}

	task, _ := s.GetTask(ctx, id)
	if task.Status != StatusPending {
		t.Errorf("僵死任务应被放回 pending，实际 %s", task.Status)
	}
	if task.TryCount != 1 {
		t.Errorf("回收不该退还重试次数（否则必然崩溃的任务会被无限重领），实际 %d", task.TryCount)
	}

	// 刚回收的任务应当立刻可领
	got, err := s.ClaimTask(ctx, "w-接班的")
	if err != nil {
		t.Fatalf("领任务出错: %v", err)
	}
	if got == nil || got.ID != id {
		t.Fatalf("回收后的任务应当立刻可领，实际 got=%v", got)
	}
	if got.TryCount != 2 {
		t.Errorf("二次领取后 try_count 应为 2，实际 %d", got.TryCount)
	}
}

// TestFailExhaustedStopsRetryLoop 验那个死循环被截住了：
// RecoverStale 把用尽重试的任务放回 pending，FailExhausted 必须把它标成失败，
// 否则它会"被领 → 失败 → 放回 → 再被领"，永远循环。
func TestFailExhaustedStopsRetryLoop(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	prefix := taskPrefix(t)
	t.Cleanup(func() { cleanupTasks(t, s, prefix) })

	id := mustCreateTask(t, s, prefix+"-exhausted", 1) // 只允许领 1 次
	if _, err := s.ClaimTask(ctx, "w1"); err != nil {
		t.Fatalf("领任务出错: %v", err)
	}
	// 直接放回排队（不退避），模拟 RecoverStale 之后的状态
	if _, err := s.DB().ExecContext(ctx,
		`UPDATE tasks SET status='pending', locked_by=NULL, locked_at=NULL WHERE id = ?`, id); err != nil {
		t.Fatalf("构造场景失败: %v", err)
	}

	n, err := s.FailExhausted(ctx)
	if err != nil {
		t.Fatalf("FailExhausted 出错: %v", err)
	}
	if n < 1 {
		t.Fatalf("应当至少截住 1 个耗尽任务，实际 %d", n)
	}
	task, _ := s.GetTask(ctx, id)
	if task.Status != StatusFailed {
		t.Errorf("重试耗尽的任务应被标为 failed，实际 %s", task.Status)
	}
	if task.Error == "" {
		t.Error("标失败时应当留下原因，便于人看到")
	}
}

// TestCancelTaskOnlyPending 验"只有排队的能取消"。
// 正在跑的任务直接改状态，会让 worker 收尾时撞上 ErrLockLost，
// 反倒看不清到底发生了什么。
func TestCancelTaskOnlyPending(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	prefix := taskPrefix(t)
	t.Cleanup(func() { cleanupTasks(t, s, prefix) })

	// pending → 可以取消
	pendingID := mustCreateTask(t, s, prefix+"-cancel-pending", 3)
	if err := s.CancelTask(ctx, pendingID); err != nil {
		t.Fatalf("取消排队中的任务应当成功，实际 %v", err)
	}
	task, _ := s.GetTask(ctx, pendingID)
	if task.Status != StatusCanceled {
		t.Errorf("取消后状态应为 canceled，实际 %s", task.Status)
	}
	// 取消掉的任务不该再被领走
	got, err := s.ClaimTask(ctx, "w-x")
	if err != nil {
		t.Fatalf("领任务出错: %v", err)
	}
	if got != nil && got.ID == pendingID {
		t.Error("已取消的任务不该被领走")
	}

	// running → 不能取消
	runningID := mustCreateTask(t, s, prefix+"-cancel-running", 3)
	if _, err := s.ClaimTask(ctx, "w-y"); err != nil {
		t.Fatalf("领任务出错: %v", err)
	}
	if err := s.CancelTask(ctx, runningID); err == nil {
		t.Error("正在执行的任务不该允许取消")
	}

	// 已是终态 → 不能取消
	if err := s.CancelTask(ctx, pendingID); err == nil {
		t.Error("已结束的任务不该允许再次取消")
	}
}

// TestGetTaskNotFound 验不存在的任务返回哨兵错误而不是裸 sql.ErrNoRows——
// 上层要靠它区分"没有这个任务"（404）和"数据库出问题了"（500）。
func TestGetTaskNotFound(t *testing.T) {
	s := testStore(t)
	if _, err := s.GetTask(context.Background(), -1); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("不存在的任务应返回 ErrTaskNotFound，实际 %v", err)
	}
}

// TestTouchLockDetectsLostLock 验续租时发现锁丢了会报错。
// worker 靠这个信号主动中止本次执行，避免和接管者同时刷同一个学号。
func TestTouchLockDetectsLostLock(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	prefix := taskPrefix(t)
	t.Cleanup(func() { cleanupTasks(t, s, prefix) })

	id := mustCreateTask(t, s, prefix+"-touch", 3)
	got, err := s.ClaimTask(ctx, "w-owner")
	if err != nil {
		t.Fatalf("领任务出错: %v", err)
	}
	// 断言领到的确实是刚建的这个任务：ClaimTask 领的是**全表最老的 pending**，
	// 不校验 id 的话，一旦表里混进别的待领行，失败会以"锁丢了"的面目出现，
	// 排查方向被带偏（这正是本用例上一次偶发失败的现象）。
	if got == nil || got.ID != id {
		t.Fatalf("应当领到自己刚建的任务 %d，实际领到 %v", id, got)
	}
	if err := s.TouchLock(ctx, id, "w-owner"); err != nil {
		t.Fatalf("自己的任务续租应当成功，实际 %v", err)
	}
	if err := s.TouchLock(ctx, id, "w-别的"); !errors.Is(err, ErrLockLost) {
		t.Fatalf("非持有者续租应返回 ErrLockLost，实际 %v", err)
	}
}

// TestTouchLockSurvivesSameMillisecondRenewal 守一个实测出来的 MySQL 陷阱：
// **RowsAffected 返回的是"实际改变的行数"，不是"匹配的行数"**。
//
// locked_at 是 DATETIME(3)，同一毫秒内连续续租两次，新的 NOW(3) 与旧的完全相等，
// MySQL 就报 0 行（实测：Rows matched: 1 / Changed: 0）。旧实现把 0 直接当丢锁，
// 于是 worker 会主动中止一个完全健康的任务——静默的错误答案。
//
// 怎么做到确定性复现：会话级的 SET timestamp 把 NOW(3) 钉死在一个固定时刻，
// 两次续租必然落在同一毫秒，不必靠运气去撞。为此要先把连接池压到 1 条——
// 会话变量跟着连接走，池子里有多条连接的话 SET 会作用在别处。
func TestTouchLockSurvivesSameMillisecondRenewal(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	prefix := taskPrefix(t)
	t.Cleanup(func() { cleanupTasks(t, s, prefix) })

	s.DB().SetMaxOpenConns(1)
	if _, err := s.DB().ExecContext(ctx, "SET timestamp = 1758770000"); err != nil {
		t.Skipf("当前 MySQL 会话不支持 SET timestamp，跳过：%v", err)
	}

	id := mustCreateTask(t, s, prefix+"-same-ms", 3)
	got, err := s.ClaimTask(ctx, "w-owner")
	if err != nil {
		t.Fatalf("领任务出错: %v", err)
	}
	if got == nil || got.ID != id {
		t.Fatalf("应当领到自己刚建的任务 %d，实际领到 %v", id, got)
	}

	// 第一次续租：把 locked_at 写成那个固定时刻
	if err := s.TouchLock(ctx, id, "w-owner"); err != nil {
		t.Fatalf("首次续租应当成功，实际 %v", err)
	}
	// 第二次：NOW(3) 还是那个固定值，MySQL 会报 Changed: 0。
	// 这一步就是旧实现出错的地方。
	if err := s.TouchLock(ctx, id, "w-owner"); err != nil {
		t.Fatalf("同一毫秒内的第二次续租被误判成丢锁了（怀疑又把 RowsAffected==0 直接当丢锁），实际 %v", err)
	}
	// 复核路径仍然要按语义工作：真的不是自己的锁，必须报错
	if err := s.TouchLock(ctx, id, "w-别人"); !errors.Is(err, ErrLockLost) {
		t.Fatalf("非持有者续租应返回 ErrLockLost，实际 %v", err)
	}
}

// ---------------------------------------------------------------------------
// 结果与事件
// ---------------------------------------------------------------------------

// TestUpsertResultKeepsOneRowPerQuestion 验重答是更新而不是插新行。
//
// 程序题会重答，同一道题被写多次是常态。靠 (task_id, pid) 唯一键
// 让数据库兜住这件事，比靠应用层每次记得判断更可靠。
func TestUpsertResultKeepsOneRowPerQuestion(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	prefix := taskPrefix(t)
	t.Cleanup(func() { cleanupTasks(t, s, prefix) })

	id := mustCreateTask(t, s, prefix+"-upsert", 3)

	first := AnswerRow{Pid: "answerForm3", Question: "1+1=?", Answer: "1", Score: 0, Tries: 1, Passed: false}
	if err := s.UpsertResult(ctx, id, first); err != nil {
		t.Fatalf("写结果失败: %v", err)
	}
	// 重答：同一个 pid，这次答对了
	second := AnswerRow{Pid: "answerForm3", Question: "1+1=?", Answer: "2", Score: 1, Tries: 2, Passed: true}
	if err := s.UpsertResult(ctx, id, second); err != nil {
		t.Fatalf("重写结果失败: %v", err)
	}
	// 另一道题
	if err := s.UpsertResult(ctx, id, AnswerRow{Pid: "answerForm4", Answer: "x", Score: 1, Tries: 1, Passed: true}); err != nil {
		t.Fatalf("写结果失败: %v", err)
	}

	rows, err := s.ListResults(ctx, id)
	if err != nil {
		t.Fatalf("读结果失败: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("同一题重答后应保持 2 行（两题各一行），实际 %d 行", len(rows))
	}
	if rows[0].Pid != "answerForm3" || rows[0].Answer != "2" || rows[0].Tries != 2 {
		t.Errorf("重答应当覆盖上一次的内容，实际 %+v", rows[0])
	}
}

// TestAppendEventSeqIsIdempotent 验重复上报同一个 seq 不会抛错也不会长出重复行。
// 事件表靠 (task_id, seq) 唯一键 + ON DUPLICATE KEY UPDATE 做到这一点。
func TestAppendEventSeqIsIdempotent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	prefix := taskPrefix(t)
	t.Cleanup(func() { cleanupTasks(t, s, prefix) })

	id := mustCreateTask(t, s, prefix+"-event", 3)

	n, err := s.MaxEventSeq(ctx, id)
	if err != nil {
		t.Fatalf("读事件序号失败: %v", err)
	}
	if n != 0 {
		t.Errorf("新任务的事件序号应为 0，实际 %d", n)
	}

	if err := s.AppendEvent(ctx, id, 1, train.Event{Kind: train.EventStarted}); err != nil {
		t.Fatalf("写事件失败: %v", err)
	}
	if err := s.AppendEvent(ctx, id, 2, train.Event{Kind: train.EventLoginOK, Detail: "登录成功"}); err != nil {
		t.Fatalf("写事件失败: %v", err)
	}
	// 重放 seq=2：应当变成幂等更新，而不是报唯一键冲突
	if err := s.AppendEvent(ctx, id, 2, train.Event{Kind: train.EventLoginOK, Detail: "登录成功（重报）"}); err != nil {
		t.Fatalf("重复上报同一个 seq 应当幂等，实际报错: %v", err)
	}

	n, _ = s.MaxEventSeq(ctx, id)
	if n != 2 {
		t.Errorf("最大序号应为 2，实际 %d", n)
	}
	events, err := s.ListEvents(ctx, id)
	if err != nil {
		t.Fatalf("读事件失败: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("事件应保持 2 条，实际 %d 条", len(events))
	}
	if events[0].Seq != 1 || events[1].Seq != 2 {
		t.Errorf("事件应按 seq 升序返回，实际 %d,%d", events[0].Seq, events[1].Seq)
	}
	if !strings.Contains(events[1].Detail, "重报") {
		t.Errorf("幂等更新应当覆盖内容，实际 %q", events[1].Detail)
	}
}

// TestClaimQueryPlan 验抢任务那条 SQL 的执行计划没有退化。
//
// 这条测试的来历值得说一下：最初 schema 里的索引是 (status, available_at, id)，
// 注释信誓旦旦写着"第三列正好是排序键，所以不会 filesort"。写测试时用 EXPLAIN
// 一验，发现完全不是那么回事——available_at 是**范围**条件，范围一生效，
// 索引内部就不再按第三列有序了，所以那个索引满足不了 ORDER BY id。
// 实测 5000 行、0 条 pending 时，优化器选的是 idx_status_created + Sort，
// idx_claim 压根没被用上。索引因此改成了 (status, id)。
//
// 所以这条测试守的是两件事：
//  1. 抢任务查询走 idx_claim，而不是扫全表——空闲轮询是最高频的操作，
//     它会随表增长而变慢，而空闲时轮询最频繁，是最容易被忽略的性能坑；
//  2. 计划里没有排序步骤（ORDER BY id 由索引满足）。
//
// 它 EXPLAIN 的是 store.go 里那条 claimSQL 本身，而不是另抄一份 SQL——
// 抄一份的话两边一不同步，测的就是一句没人用的语句。
func TestClaimQueryPlan(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	rows, err := s.DB().QueryContext(ctx, "EXPLAIN "+claimSQL)
	if err != nil {
		t.Fatalf("EXPLAIN 失败: %v", err)
	}
	defer rows.Close()

	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("读列名失败: %v", err)
	}
	if !rows.Next() {
		t.Fatal("EXPLAIN 没有返回任何行")
	}
	vals := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := rows.Scan(ptrs...); err != nil {
		t.Fatalf("读 EXPLAIN 结果失败: %v", err)
	}

	cell := func(name string) string {
		for i, c := range cols {
			if c == name {
				if b, ok := vals[i].([]byte); ok {
					return string(b)
				}
				return fmt.Sprint(vals[i])
			}
		}
		return ""
	}

	if possible := cell("possible_keys"); !strings.Contains(possible, "idx_claim") {
		t.Errorf("idx_claim 应当可用于抢任务查询，实际 possible_keys=%q", possible)
	}
	if key := cell("key"); key != "idx_claim" {
		t.Errorf("抢任务查询应当走 idx_claim，实际走了 %q；"+
			"这条查询每轮轮询都会执行，退化会随表增长（见 schema.sql 索引 1 的实测数据）", key)
	}
	if extra := cell("Extra"); strings.Contains(extra, "filesort") {
		t.Errorf("抢任务查询不该产生 filesort（status 等值 + 索引内 id 有序即可满足 ORDER BY id），"+
			"实际 Extra=%q", extra)
	}
}

// ---------------------------------------------------------------------------
// 加密
// ---------------------------------------------------------------------------

func TestPasswordRoundTrip(t *testing.T) {
	secret := strings.Repeat("s", 32)
	const plain = "我的密码 p@ss w0rd！"
	enc, err := EncryptPassword(secret, plain)
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	if bytes.Contains(enc, []byte(plain)) {
		t.Error("密文里不该出现明文——加密没生效")
	}
	got, err := DecryptPassword(secret, enc)
	if err != nil {
		t.Fatalf("解密失败: %v", err)
	}
	if got != plain {
		t.Errorf("解密结果不一致：期望 %q，实际 %q", plain, got)
	}
}

// TestEncryptUsesFreshNonce 验每次加密的密文都不同。
//
// GCM 的 nonce 在同一密钥下绝不允许重复：重复会让攻击者把两条密文异或，
// 直接还原出明文。所以 nonce 必须每次由 crypto/rand 新取，
// 哪怕明文一模一样，密文也必须不同。
func TestEncryptUsesFreshNonce(t *testing.T) {
	secret := strings.Repeat("s", 32)
	a, _ := EncryptPassword(secret, "同一个密码")
	b, _ := EncryptPassword(secret, "同一个密码")
	if bytes.Equal(a, b) {
		t.Error("两次加密同一明文得到了相同密文，说明 nonce 没有随机化")
	}
	// 长度应当是 nonce(12) + 密文 + tag(16)
	if len(a) != 12+len("同一个密码")+16 {
		t.Errorf("密文长度应为 12+明文+16，实际 %d", len(a))
	}
}

// TestDecryptRejectsWrongSecretOrTampering 验认证加密的两个用处：
// 密钥不对解不开；密文被改过一个字节也解不开（GCM 会校验 tag）。
func TestDecryptRejectsWrongSecretOrTampering(t *testing.T) {
	secret := strings.Repeat("s", 32)
	enc, _ := EncryptPassword(secret, "原始密码")

	if _, err := DecryptPassword(strings.Repeat("k", 32), enc); err == nil {
		t.Error("密钥不对时应当解密失败")
	}

	tampered := append([]byte(nil), enc...)
	tampered[len(tampered)-1] ^= 0x01 // 翻转 tag 的最后一 bit
	if _, err := DecryptPassword(secret, tampered); err == nil {
		t.Error("密文被篡改后应当解密失败（GCM 的认证性被绕过）")
	}

	if _, err := DecryptPassword(secret, []byte("太短")); err == nil {
		t.Error("长度不合法的密文应当解密失败，而不是 panic")
	}
}

// ---------------------------------------------------------------------------
// 配置
// ---------------------------------------------------------------------------

// fakeEnv 把 map 包成 LoadConfig 需要的 getenv 函数。
func fakeEnv(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

func TestLoadConfigDefaultsAndValid(t *testing.T) {
	cfg, err := LoadConfig(fakeEnv(map[string]string{"TASK_SECRET": strings.Repeat("s", 32)}))
	if err != nil {
		t.Fatalf("合法配置不应报错: %v", err)
	}
	if cfg.Addr != "127.0.0.1:8080" {
		t.Errorf("默认监听地址应是 127.0.0.1:8080（只监听本机，见 README 的安全说明），实际 %s", cfg.Addr)
	}
	if cfg.WorkerID == "" {
		t.Error("没传 WORKER_ID 时应当自动生成一个（主机名-pid），否则无法区分是谁领走了任务")
	}
	if cfg.LockTimeout <= cfg.TaskMaxRuntime {
		t.Errorf("默认值也要满足 锁超时(%s) > 任务超时(%s)", cfg.LockTimeout, cfg.TaskMaxRuntime)
	}
	if !strings.Contains(cfg.DSN(), "parseTime=true") {
		t.Errorf("DSN 必须带 parseTime=true，否则 DATETIME 扫不进 time.Time: %s", cfg.DSN())
	}
	if !strings.Contains(cfg.DSN(), "charset=utf8mb4") {
		t.Errorf("DSN 必须指定 utf8mb4，否则题干里的 emoji 会变成问号: %s", cfg.DSN())
	}
}

// TestLoadConfigRejectsMissingSecret 验"缺密钥就起不来"。
//
// 这里不能做降级（比如"没有密钥就先存明文"）——那样跑起来的服务，
// 库里躺着的是真实学生的密码明文。
func TestLoadConfigRejectsMissingSecret(t *testing.T) {
	_, err := LoadConfig(fakeEnv(nil))
	if err == nil {
		t.Fatal("缺少 TASK_SECRET 时必须启动失败")
	}
	if !strings.Contains(err.Error(), "TASK_SECRET") {
		t.Errorf("错误信息要指名道姓说缺哪个变量，实际 %q", err.Error())
	}
}

func TestLoadConfigRejectsShortSecret(t *testing.T) {
	_, err := LoadConfig(fakeEnv(map[string]string{"TASK_SECRET": "太短了"}))
	if err == nil {
		t.Fatal("TASK_SECRET 过短时必须启动失败")
	}
	if !strings.Contains(err.Error(), "32") {
		t.Errorf("错误信息应当说明最小长度，实际 %q", err.Error())
	}
}

// TestLoadConfigRejectsLockTimeoutTooShort 验那个很容易配错、后果又很隐蔽的约束。
// 锁超时 <= 任务超时的话，还在正常执行的任务会被当成僵死任务抢走重跑。
func TestLoadConfigRejectsLockTimeoutTooShort(t *testing.T) {
	_, err := LoadConfig(fakeEnv(map[string]string{
		"TASK_SECRET":      strings.Repeat("s", 32),
		"LOCK_TIMEOUT":     "5m",
		"TASK_MAX_RUNTIME": "15m",
	}))
	if err == nil {
		t.Fatal("LOCK_TIMEOUT 小于 TASK_MAX_RUNTIME 时必须启动失败")
	}
	if !strings.Contains(err.Error(), "LOCK_TIMEOUT") {
		t.Errorf("错误信息应当点明是哪两个配置冲突了，实际 %q", err.Error())
	}
}

// TestConfigRedactedHidesSecrets 验日志用的摘要不含任何凭据。
func TestConfigRedactedHidesSecrets(t *testing.T) {
	secret := strings.Repeat("S", 32)
	cfg, _ := LoadConfig(fakeEnv(map[string]string{
		"TASK_SECRET": secret,
		"DB_PASSWORD": "db-secret-pw",
	}))
	got := cfg.Redacted()
	if strings.Contains(got, secret) {
		t.Error("脱敏摘要里出现了加密密钥——它是会被打印进日志的")
	}
	if strings.Contains(got, "db-secret-pw") {
		t.Error("脱敏摘要里出现了数据库口令")
	}
}

func TestEnvDurationAcceptsBothForms(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
	}{
		{"30s", 30 * time.Second}, // Go 的时长写法
		{"5m", 5 * time.Minute},   // 同上
		{"45", 45 * time.Second},  // 纯秒数，迁就习惯写数字的运维配置
		{"", 7 * time.Second},     // 空 → 默认值
		{"不是数字", 7 * time.Second}, // 解释不了 → 默认值，而不是崩掉
	}
	for _, c := range cases {
		got := envDuration(fakeEnv(map[string]string{"K": c.in}), "K", 7*time.Second)
		if got != c.want {
			t.Errorf("envDuration(%q) = %s，期望 %s", c.in, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// 状态机与错误分类
// ---------------------------------------------------------------------------

func TestTaskStatusTerminal(t *testing.T) {
	terminal := []TaskStatus{StatusSucceeded, StatusFailed, StatusCanceled}
	notTerminal := []TaskStatus{StatusPending, StatusRunning}
	for _, st := range terminal {
		if !st.Terminal() {
			t.Errorf("%q 应当是终态", st)
		}
	}
	for _, st := range notTerminal {
		if st.Terminal() {
			t.Errorf("%q 不该被当作终态", st)
		}
	}
}

// TestRetryableClassifiesErrors 验错误分类。
//
// 分错的代价是实打实的：把"需要人工登录"当可重试，队列会为一件注定失败的事
// 反复起浏览器；把临时故障当不可重试，一次随机失败就白丢一个任务。
//
// 其中 context.Canceled 那一组是端到端验证时补上来的：**同样的取消错误，
// 是我方取消还是上游取消，结论完全相反**。只按错误类型分类会把
// "浏览器自己崩了"误判成"我们放弃了"。
func TestRetryableClassifiesErrors(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		ctxErr error // 我方执行 ctx 的状态；nil 表示我们没取消
		want   bool
	}{
		{"成功没有错误", nil, nil, false},

		{"需要人工登录", train.ErrManualLoginRequired, nil, false},
		{"被包装过的人工登录错误", fmt.Errorf("刷题失败: %w", train.ErrManualLoginRequired), nil, false},

		// 浏览器调试资源被占：占位者不会因为我们要重试就消失，重试只是白等一轮超时
		{"端口/profile 被另一个 Chrome 占用", train.ErrBrowserBusy, nil, false},
		{"被包装过的占用错误", fmt.Errorf("启动浏览器失败: %w", train.ErrBrowserBusy), nil, false},
		// 但"浏览器没能暴露调试端口、且没有占用证据"是另一回事——仍算临时故障
		{"浏览器没暴露调试端口（无占用证据）", errors.New("等待 15s 仍未读到浏览器的调试地址"), nil, true},

		// 我方取消：时间预算花完/服务要关/锁已丢，重试毫无意义
		{"我方超时", fmt.Errorf("建立连接失败: %w", context.DeadlineExceeded), context.DeadlineExceeded, false},
		{"我方主动取消", context.Canceled, context.Canceled, false},

		// 上游取消：不是我们干的，多半是浏览器崩了 → 临时故障，值得重试
		{"上游连接断开（chromedp 报 context canceled）", fmt.Errorf("建立浏览器控制连接失败: %w", context.Canceled), nil, true},
		{"上游自己的超时", fmt.Errorf("请求超时: %w", context.DeadlineExceeded), nil, true},

		{"验证码识别失败（站点抖动）", errors.New("验证码识别失败"), nil, true},
		{"网络抖动", errors.New("connection reset by peer"), nil, true},
	}
	for _, c := range cases {
		if got := retryable(c.err, c.ctxErr); got != c.want {
			t.Errorf("%s: retryable(%v, ctxErr=%v) = %v，期望 %v",
				c.name, c.err, c.ctxErr, got, c.want)
		}
	}
}

func TestRetryDelayGrows(t *testing.T) {
	w := &Worker{}
	if w.retryDelay(1) != 30*time.Second {
		t.Errorf("第一次失败后退避 30s，实际 %s", w.retryDelay(1))
	}
	if w.retryDelay(3) <= w.retryDelay(2) {
		t.Error("退避应当随失败次数变大（给上游更多恢复时间）")
	}
	if w.retryDelay(0) != 30*time.Second {
		t.Errorf("异常输入不该产出 0 延迟（那等于没有退避），实际 %s", w.retryDelay(0))
	}
}

func TestMaskUsername(t *testing.T) {
	cases := map[string]string{
		"2023211234": "2023****",
		"1234":       "****",
		"123":        "****",
		"":           "****",
	}
	for in, want := range cases {
		if got := maskUsername(in); got != want {
			t.Errorf("maskUsername(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

// TestTruncateRunesIsRuneSafe 这条是 v1.2.0 踩过的坑的回归测试：
// s[:n] 按字节切，一个中文 3 字节，切在中间就得到半个字符（写进库变乱码）。
func TestTruncateRunesIsRuneSafe(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"abcdef", 3, "abc"},
		{"abcdef", 6, "abcdef"},
		{"abcdef", 10, "abcdef"},
		{"", 3, ""},
		{"中文测试", 2, "中文"},
		{"中文测试", 4, "中文测试"},
		{"中a文b", 3, "中a文"},
	}
	for _, c := range cases {
		got := truncateRunes(c.in, c.n)
		if got != c.want {
			t.Errorf("truncateRunes(%q, %d) = %q，期望 %q", c.in, c.n, got, c.want)
		}
	}
	// 截断结果必须仍是合法 UTF-8，否则说明又按字节切了
	chars := truncateRunes("中文字符测试", 3)
	if !json.Valid([]byte(`"` + chars + `"`)) {
		t.Errorf("截断后应当仍是合法 UTF-8，实际 %q", chars)
	}
}

func TestNullIfEmpty(t *testing.T) {
	if nullIfEmpty("") != nil {
		t.Error("空串应当转成 NULL（NULL 表示「没出错」，空串是含糊的「出错了但没说什么」）")
	}
	if nullIfEmpty("boom") != "boom" {
		t.Error("非空串应当原样返回")
	}
}

// ---------------------------------------------------------------------------
// HTTP 层：不外泄字段、URL 匹配、中间件
// ---------------------------------------------------------------------------

// TestViewTaskNeverExposesPassword 验对外视图里根本没有密码字段。
//
// 这条测试用了反射：它不是"检查某个字段没被赋值"，而是检查
// **结构体里压根不存在这个字段**——这样即便以后有人给 taskView 加上
// PasswordEnc，测试也会立刻失败。
func TestViewTaskNeverExposesPassword(t *testing.T) {
	secret := strings.Repeat("s", 32)
	enc, _ := EncryptPassword(secret, "真实密码明文")
	task := &Task{
		ID: 7, Username: "2023001", Mode: train.ModeQuiz, Num: 5,
		Status: StatusRunning, PasswordEnc: enc, CreatedAt: time.Now(),
	}

	v := viewTask(task, []EventRow{{Seq: 1, Kind: train.EventStarted, At: time.Now()}})
	blob, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("序列化视图失败: %v", err)
	}
	if bytes.Contains(blob, enc) {
		t.Error("JSON 里出现了密码密文")
	}
	if bytes.Contains(blob, []byte("真实密码明文")) {
		t.Error("JSON 里出现了密码明文")
	}

	// 结构上就没有这个字段，而不是"这次忘了填"
	if _, ok := reflect.TypeOf(v).FieldByName("PasswordEnc"); ok {
		t.Error("taskView 不该包含 PasswordEnc 字段：内外结构分离的意义就是让泄漏在结构上不可能")
	}
	taskType := reflect.TypeOf(*task) // 解引用，拿到结构体类型而不是指针类型
	if _, ok := taskType.FieldByName("PasswordEnc"); !ok {
		t.Error("store.Task 里应当有 PasswordEnc（它是内部结构），测试前提已变，请检查")
	}
}

// TestHandlerRouting 验路由表按预期工作：未知路径 404、方法不匹配 405。
// 这些都由 http.ServeMux 负责，但路由写错（比如漏了方法前缀）时
// 表现是"所有请求都 404"，没有测试就只能靠肉眼读路由表。
func TestHandlerRouting(t *testing.T) {
	// 这些请求都打在路由匹配之前就会返回，不需要真的连数据库
	s := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	h := s.Handler()

	cases := []struct {
		method, path string
		want         int
	}{
		{"GET", "/不存在的路径", http.StatusNotFound},
		{"POST", "/healthz", http.StatusMethodNotAllowed}, // /healthz 只允许 GET
		{"GET", "/api/tasks/abc", http.StatusBadRequest},  // id 必须是正整数
		{"GET", "/api/tasks/0", http.StatusBadRequest},    // id 必须为正
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s %s 期望 %d，实际 %d", c.method, c.path, c.want, rec.Code)
		}
	}
}

// TestRecoverMWTurnsPanicInto500 验 panic 被兜住、且不回显内部信息。
func TestRecoverMWTurnsPanicInto500(t *testing.T) {
	s := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	h := s.recoverMW(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("数据库连接串里有个不该外泄的秘密")
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("panic 应当变成 500，实际 %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "不该外泄的秘密") {
		t.Error("响应体里不该回显 panic 内容：它可能带路径、变量值等内部信息")
	}
}

// TestTimeoutMWWrites504WhenNothingWritten 验那个"注释说会 504"的承诺。
//
// 只挂 ctx 是不够的：handler 得自己检查才会被打断。真正的兜底是
// 中间件在超时后补一个响应，否则客户端会一直挂着等。
func TestTimeoutMWWrites504WhenNothingWritten(t *testing.T) {
	s := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	h := s.timeoutMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 模拟"卡在数据库上"：等 ctx 被取消，且一个字都没写
		<-r.Context().Done()
	}), 50*time.Millisecond)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))

	if rec.Code != http.StatusGatewayTimeout {
		t.Errorf("超时且未写响应时应返回 504，实际 %d", rec.Code)
	}
}

// TestTimeoutMWDoesNotAppendAfterWrite 验已经写过响应就不再补 504，
// 否则会把一个正常的响应体后面追加一段 JSON，两边都坏掉。
func TestTimeoutMWDoesNotAppendAfterWrite(t *testing.T) {
	s := &Server{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	h := s.timeoutMW(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
		<-r.Context().Done() // 写完之后才超时
	}), 50*time.Millisecond)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("已经写过响应的请求不该被改成 504，实际 %d", rec.Code)
	}
	if got := rec.Body.String(); got != `{"ok":true}` {
		t.Errorf("响应体不该被追加内容，实际 %q", got)
	}
}

// TestProgressPageEscapesUserData 验进度页对来自数据库的字段做了转义。
//
// 学号是用户可控的（提交任务时填什么就是什么），错误信息里也可能带上
// 页面内容。不转义的话，一个学号填成 <script> 就是一次存储型 XSS——
// 而且它会被存进库里，每次打开进度页都触发。
//
// 这里刻意走 viewTask 再渲染，跟 handler 的实际路径保持一致：
// 直接把 store.Task 丢给模板（最初的写法）虽然转义照样生效，
// 但渲染出来的时间格式和 JSON 接口不一样，那是另一个 bug（见下面那条断言）。
func TestProgressPageEscapesUserData(t *testing.T) {
	evil := `<script>alert('xss')</script>`
	task := &Task{
		ID: 1, Username: evil, Mode: train.ModeQuiz, Num: 3,
		Status: StatusFailed, Error: evil,
		CreatedAt: time.Date(2026, 9, 25, 11, 27, 37, 383_000_000, time.FixedZone("CST", 8*3600)),
	}

	var buf bytes.Buffer
	if err := progressTmpl.Execute(&buf, map[string]any{
		"Tasks": []taskView{viewTask(task, nil)}, "Counts": map[string]int{"failed": 1},
	}); err != nil {
		t.Fatalf("渲染进度页失败: %v", err)
	}
	html := buf.String()
	if strings.Contains(html, "<script>") {
		t.Error("渲染结果里出现了未转义的 <script>，进度页存在存储型 XSS")
	}
	if !strings.Contains(html, "&lt;script&gt;") {
		t.Error("应当看到被转义后的内容，说明 html/template 的自动转义没生效")
	}
	// 页面上的时间格式必须和 JSON 接口一致。
	// 最初的写法是直接把 []*store.Task 交给模板，于是页面显示的是
	// "2026-09-25 11:27:37.383 +0800 CST"，而 JSON 里是 "2026-09-25 11:27:37"。
	if !strings.Contains(html, "2026-09-25 11:27:37") {
		t.Error("进度页应当显示与 JSON 接口一致的创建时间格式")
	}
	if strings.Contains(html, "+0800 CST") {
		t.Error("进度页渲染了裸的 time.Time（带时区和纳秒），说明页面没走 viewTask")
	}
}

// TestProgressPageRendersEmptyState 验没有任务时页面仍然能渲染——
// 一个空列表导致的模板错误会让首页直接 500，而这恰恰是新部署的第一印象。
func TestProgressPageRendersEmptyState(t *testing.T) {
	var buf bytes.Buffer
	if err := progressTmpl.Execute(&buf, map[string]any{
		"Tasks": nil, "Counts": map[string]int{},
	}); err != nil {
		t.Fatalf("空列表也应当能渲染: %v", err)
	}
	if !strings.Contains(buf.String(), "还没有任务") {
		t.Error("空列表应当给出提示文案，而不是一张空表")
	}
}

// ---------------------------------------------------------------------------
// 配置文件的加载：区分「不存在」与「存在但读不了」
//
// 这一组守的又是同一类问题——**不报错的失败**。旧实现把 godotenv.Load 的任何错误
// 都当成"换下一个候选文件试试"，于是格式坏掉的 server.env 会被整个跳过，
// 用户看到的是"我明明写了 TASK_SECRET，程序却说必填"，日志里却不提文件有问题。
// ---------------------------------------------------------------------------

// unsetEnvForTest 保证某个环境变量在用例开始前是空的，并在结束时恢复原状。
// 不能直接用 t.Setenv：godotenv.Load **不覆盖已存在的环境变量**，先把变量设上就测不出效果了。
func unsetEnvForTest(t *testing.T, key string) {
	t.Helper()
	prev, had := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(key, prev)
			return
		}
		_ = os.Unsetenv(key)
	})
}

func TestLoadEnvFileSkipsMissingFiles(t *testing.T) {
	// 空目录：三个候选文件都不存在。没配置不算错误，
	// 后面 Config 校验会用「TASK_SECRET 必填」把话说清楚。
	t.Chdir(t.TempDir())
	if err := loadEnvFile(); err != nil {
		t.Fatalf("文件都不存在时不应当报错，实际：%v", err)
	}
}

func TestLoadEnvFileLoadsExistingFile(t *testing.T) {
	const key = "CQUPT_TEST_LOADENV_MARKER"
	unsetEnvForTest(t, key)

	dir := t.TempDir()
	content := "# 注释行\n\n" + key + "=from-server-env\n"
	if err := os.WriteFile(filepath.Join(dir, "server.env"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	if err := loadEnvFile(); err != nil {
		t.Fatalf("正常配置不应当报错，实际：%v", err)
	}
	if got := os.Getenv(key); got != "from-server-env" {
		t.Errorf("应当从 server.env 读到 %q，实际 %q", "from-server-env", got)
	}
}

// TestLoadEnvFileRejectsBOMPrefixedFile 用一个实测过的失败现场做用例：
// Windows PowerShell 5.1 的 `Out-File -Encoding utf8` 会写进 UTF-8 BOM，
// godotenv 解析首行时报 `unexpected character "»" in variable name`。
// 关键不是"会报错"，而是**这个错误必须被抛出来**，不能被当成"换下一个文件"吃掉。
func TestLoadEnvFileRejectsBOMPrefixedFile(t *testing.T) {
	dir := t.TempDir()
	bom := append([]byte{0xEF, 0xBB, 0xBF}, []byte("TASK_SECRET=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n")...)
	if err := os.WriteFile(filepath.Join(dir, "server.env"), bom, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)

	err := loadEnvFile()
	if err == nil {
		t.Fatal("带 UTF-8 BOM 的 server.env 应当报错——静默跳过它，用户只会看到一句莫名其妙的「TASK_SECRET 必填」")
	}
	if !strings.Contains(err.Error(), "server.env") {
		t.Errorf("错误信息里应当点出是哪个文件有问题，实际：%v", err)
	}
	if !strings.Contains(err.Error(), "BOM") {
		t.Errorf("错误信息里应当给出最可能的原因（UTF-8 BOM），实际：%v", err)
	}
}
