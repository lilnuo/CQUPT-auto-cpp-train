package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"cqupt/train"
)

// ============================================================================
// worker.go —— 从队列里领任务并执行
//
// worker 是串行的：一次只跑一个任务。这不是没来得及做并发，而是被物理条件
// 限死的，也正是"为什么必须排队"的答案：
//
//   1. Chrome 对同一个 profile 目录有单实例锁。两个进程共用 .chrome-profile
//      时，后启动的那个只会把网址丢给已有实例，然后自己退出——调试端口没开，
//      CDP 根本连不上。
//   2. CDP 调试端口是固定的一个。两个任务并发就要分配端口，还要给它做生命周期管理。
//   3. 同一个学号只能有一个登录会话。同一个账号在两个浏览器里同时登录，
//      后登录的会把前一个踢下线，两边都乱套。
//   4. 这套流程本身很重：起一个真实浏览器、等 20 秒过 WAF 挑战、跑 OCR、
//      再逐题调大模型。单机并发两三个就已经不是 CPU 瓶颈而是"站点会不会
//      判定异常"的问题了。
//
// 所以正确的做法不是在 worker 里硬塞并发，而是**让请求方排队**：
// HTTP 层立刻返回任务 id，客户端拿着 id 轮询进度。用户体感上"提交即返回"，
// 排队这件事被藏进了数据库里。
// ============================================================================

// Worker 从数据库队列领取任务并执行。
type Worker struct {
	store *Store
	cfg   Config
	log   *slog.Logger
}

// NewWorker 构造一个 worker。
func NewWorker(store *Store, cfg Config, log *slog.Logger) *Worker {
	return &Worker{store: store, cfg: cfg, log: log}
}

// Run 循环领取任务直到 ctx 被取消。
//
// 关闭服务时的语义是**排空（drain）**而不是**中断**：ctx 取消后不再领新任务，
// 但手上正在跑的那个会被跑完。原因很直接——一次刷题要起浏览器、过 WAF、
// 调大模型，跑到一半被砍掉的话，context.Canceled 在 retryable 里是"不可重试"，
// 这次尝试就白费了；而让它跑完只是让进程多活几分钟。
//
// 所以执行任务的父 ctx 用 WithoutCancel 剥掉了取消信号，它只受 TaskMaxRuntime 约束。
// 硬性终止交给进程退出：真到了必须立刻关的时候，任务停在 running，
// 下次启动由锁超时 + RecoverStale 把它捞回来——这条兜底路径本来就必须存在。
func (w *Worker) Run(ctx context.Context) {
	w.log.Info("worker 启动，开始监听队列", "标识", w.cfg.WorkerID)
	taskCtx := context.WithoutCancel(ctx)
	for {
		if ctx.Err() != nil {
			w.log.Info("worker 收到停止信号，退出", "说明", "不再领新任务")
			return
		}

		ran, err := w.runOnce(ctx, taskCtx)
		if err != nil {
			w.log.Error("处理任务出错", "err", err)
			// 出错也要退一下，否则数据库挂了会在这里打转刷满日志
			if !sleep(ctx, w.cfg.WorkerPollInterval) {
				return
			}
			continue
		}
		if ran {
			// 刚干完一个，立刻接着看还有没有活——不睡，避免平白多等一个轮询周期
			continue
		}

		// 队列空。趁这个空档做两件维护工作。
		if freed, err := w.store.RecoverStale(ctx, w.cfg.LockTimeout); err != nil {
			w.log.Error("回收僵死任务失败", "err", err)
		} else if freed > 0 {
			w.log.Warn("发现僵死任务，已放回队列", "数量", freed)
			continue // 刚放回去的立刻就能领，别睡
		}
		if failed, err := w.store.FailExhausted(ctx); err != nil {
			w.log.Error("标记重试耗尽任务失败", "err", err)
		} else if failed > 0 {
			w.log.Warn("有任务重试次数已用尽，标记为失败", "数量", failed)
		}

		if !sleep(ctx, w.cfg.WorkerPollInterval) {
			return
		}
	}
}

// runOnce 尝试领一个任务并执行。
// 返回 ran=false 表示队列空（不是错误）。
//
// claimCtx 用于队列操作（可被关闭信号打断），taskCtx 用于执行任务（关闭时不打断）。
func (w *Worker) runOnce(claimCtx, taskCtx context.Context) (bool, error) {
	t, err := w.store.ClaimTask(claimCtx, w.cfg.WorkerID)
	if err != nil {
		return false, err
	}
	if t == nil {
		return false, nil
	}
	w.execute(taskCtx, t)
	return true, nil
}

// execute 执行一个已领取的任务，并负责把它推进到终态。
//
// 这个函数的每条 return 路径都必须保证"任务最终被写成一个终态，或者被退回队列"。
// 漏掉任何一条，任务就会永远停在 running——而 running 的任务谁都不会去碰它，
// 直到锁超时被回收。所以收尾逻辑集中在 defer 里做，而不是靠每个分支自觉。
func (w *Worker) execute(parent context.Context, t *Task) {
	log := w.log.With(
		"task_id", t.ID,
		"学号", maskUsername(t.Username),
		"模式", t.Mode,
		"第几次领取", t.TryCount,
	)

	// 给这次执行套一个总超时。超时后 ctx 取消，chromedp 的操作会随之失败并返回，
	// 于是我们能拿回控制权、把任务标成失败，而不是无限期挂在浏览器上。
	ctx, cancel := context.WithTimeout(parent, w.cfg.TaskMaxRuntime)
	defer cancel()

	// 续租心跳：定期把 locked_at 往前推，避免被别的 worker 当成僵死任务抢走重跑。
	hbCtx, stopHeartbeat := context.WithCancel(ctx)
	defer stopHeartbeat()
	go w.heartbeat(hbCtx, t.ID, cancel, log)

	// 解密密码。密文解不开说明密钥换了或数据被改过，这类任务重试一百次也一样，
	// 所以直接判失败，不浪费一次重试配额。
	password, err := DecryptPassword(w.cfg.TaskSecret, t.PasswordEnc)
	if err != nil {
		w.finish(parent, t, nil, "密码解密失败: "+err.Error(), log)
		return
	}

	onEvent := w.eventRecorder(parent, t.ID, log)

	res, runErr := train.Run(ctx, train.Request{
		Username: t.Username,
		Password: password,
		Num:      t.Num,
		Mode:     t.Mode,
	}, train.Options{
		// 无人值守：不能停下来等人手动登录（那会把唯一的 worker 永久占住）
		AllowManualLogin: false,
		OnEvent:          onEvent,
		// 带任务标识的 logger 会在运行期间成为默认 logger，
		// 于是 train 内部散落各处的日志自动带上 task_id，不会多条任务混在一起分不清
		Logger: log,
	})

	// 题目结果落库。无论成功失败都写：失败时那一部分结果恰恰是最有价值的排查线索。
	if len(res.Questions) > 0 {
		w.saveResults(parent, t.ID, res.Questions, log)
	}

	if runErr == nil {
		score := res.Score
		w.finish(parent, t, &score, "", log)
		log.Info("任务完成", "总分", score, "完成题数", len(res.Questions))
		return
	}

	// 失败：先看还该不该重试。
	// 传进去的第二个参数是"我们自己有没有取消这次执行"——判断依据见 retryable 的注释。
	if retryable(runErr, ctx.Err()) && t.TryCount < t.MaxTry {
		delay := w.retryDelay(t.TryCount)
		if err := w.store.ReleaseForRetry(parent, t.ID, w.cfg.WorkerID, delay, runErr.Error()); err != nil {
			log.Error("退回队列失败", "err", err)
			return
		}
		log.Warn("任务失败，稍后重试", "err", runErr, "重试间隔", delay, "已用", t.TryCount, "上限", t.MaxTry)
		return
	}

	w.finish(parent, t, nil, runErr.Error(), log)
	log.Error("任务最终失败", "err", runErr,
		"是否可重试", retryable(runErr, ctx.Err()), "我方 ctx 状态", ctx.Err())
}

// heartbeat 定期续租。一旦发现锁已经不在自己手上，就主动取消本次执行。
//
// 为什么要主动取消：锁丢了意味着别的 worker 可能已经在跑同一个任务了。
// 继续跑下去最坏的结果是同一个学号被两个浏览器同时登录、互相踢会话，
// 两边都拿到错乱的结果。宁可这边立刻停下。
func (w *Worker) heartbeat(ctx context.Context, taskID int64, cancel context.CancelFunc, log *slog.Logger) {
	// 续租间隔取锁超时的 1/3：留出两次容错余地，即便某一次写库抖动，
	// 也不会立刻触到超时线。下限 10 秒是防止锁超时配得太短时把数据库打爆。
	interval := w.cfg.LockTimeout / 3
	if interval < 10*time.Second {
		interval = 10 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := w.store.TouchLock(ctx, taskID, w.cfg.WorkerID); err != nil {
				log.Error("续租失败，主动中止本次任务", "err", err)
				cancel()
				return
			}
		}
	}
}

// eventRecorder 返回一个把进度事件写库的回调。
//
// seq 在这里递增：一个任务只由一个 worker 执行，所以进程内计数器就是安全的。
// 起始值从库里读 MAX(seq)，这样即便任务是被回收后重跑的，编号也能接着往下走，
// 不会和上一轮的事件撞号。
func (w *Worker) eventRecorder(ctx context.Context, taskID int64, log *slog.Logger) func(train.Event) {
	seq, err := w.store.MaxEventSeq(ctx, taskID)
	if err != nil {
		log.Warn("读取事件序号失败，从 0 开始编号", "err", err)
	}
	var mu sync.Mutex

	return func(e train.Event) {
		mu.Lock()
		seq++
		mySeq := seq
		mu.Unlock()

		// 事件落库失败不该影响刷题本身——它是"锦上添花"的数据。
		// 但也不能完全不报，所以降级为 Warn，并且不影响任务的成败判定。
		// 用独立的小超时：任务已经超时取消时，仍然要让最后几条事件写进去。
		wctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()

		if err := w.store.AppendEvent(wctx, taskID, mySeq, e); err != nil {
			log.Warn("写入进度事件失败", "err", err, "事件", e.Kind)
		}
		if e.Kind == train.EventQuestionDone {
			if err := w.store.BumpQuestionDone(wctx, taskID); err != nil {
				log.Warn("更新完成题数失败", "err", err)
			}
		}
		log.Info("进度", "事件", e.Kind, "题", e.Pid, "得分", e.Score, "说明", e.Detail)
	}
}

// saveResults 把每道题的结果写入结果表。
func (w *Worker) saveResults(ctx context.Context, taskID int64, qs []train.QuestionResult, log *slog.Logger) {
	for _, q := range qs {
		row := AnswerRow{
			Pid:      q.Pid,
			Question: q.Text,
			Answer:   q.Answer,
			Score:    q.Score,
			Tries:    q.Tries,
			Passed:   q.Passed(),
		}
		if err := w.store.UpsertResult(ctx, taskID, row); err != nil {
			log.Warn("写入题目结果失败", "err", err, "题", q.Pid)
		}
	}
}

// finish 写终态。用独立的短超时，因为走到这里时 ctx 可能已经因为任务超时被取消了——
// 而"把失败写进数据库"恰恰是超时情况下最必须完成的一步。
func (w *Worker) finish(ctx context.Context, t *Task, score *int, errMsg string, log *slog.Logger) {
	status := StatusSucceeded
	if errMsg != "" {
		status = StatusFailed
	}
	// 用 context.WithoutCancel 剥掉父 ctx 的取消信号，只保留值。
	// 否则任务一超时，收尾这条 UPDATE 会直接失败，任务就永远卡在 running。
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()

	if err := w.store.FinishTask(fctx, t.ID, w.cfg.WorkerID, status, score, errMsg); err != nil {
		log.Error("任务收尾失败，任务可能停留在 running 状态等回收", "err", err)
	}
}

// retryDelay 是重试退避：第 N 次失败后等 N*30 秒再重来。
//
// 退避不只是"省资源"，更是为了让**上游有时间恢复**：验证码识别失败往往是因为
// 站点当时在抖，或者 WAF 挑战正忙。立刻重试大概率撞上同一个坑。
func (w *Worker) retryDelay(tryCount int) time.Duration {
	if tryCount < 1 {
		tryCount = 1
	}
	return time.Duration(tryCount) * 30 * time.Second
}

// retryable 判断这个错误还值不值得重试。
//
// 分类原则：只把"再来一次可能就好了"的算可重试。
//
// 第二个参数 ctxErr 是**我们自己**那个执行 ctx 的状态（ctx.Err()）。它必须传进来，
// 因为光看错误本身分不清两种 context 取消：
//
//   - 我方取消（ctxErr != nil）：任务超时了、服务要关了、或者续租发现锁丢了。
//     这三种情况下重试都毫无意义——时间预算已经花完，或者这次尝试早就作废了。
//
//   - 上游取消（ctxErr == nil 却报了 context.Canceled）：这不是我们干的，
//     多半是浏览器进程自己崩了、或者 CDP 连接断了。chromedp 在底层连接断开时
//     报的就是 "context canceled"。这种属于**临时故障**，正是最该重试的一类。
//
//     （这条区分是端到端验证时发现的：任务以 "建立浏览器控制连接失败: context canceled"
//     失败，却被判为不可重试，一次尝试都没重试就终态了。原因是上游的取消信号
//     被误当成了我们自己的取消。）
//
// 另外几类明确的永久失败：
//   - 需要人工登录：无人值守环境下重试一百次还是需要人工登录，
//     这是**确定性**结论，不是运气问题。
//   - 浏览器调试资源被另一个 Chrome 占用（端口或 profile）：
//     占位者不会因为我们要重试就消失，重试只会再等一轮超时、
//     再在用户屏幕上弹一次窗口。
//
// 其余（验证码识别失败、网络抖动、页面没渲染出来）都算可重试。
//
// 这里还剩一个诚实的局限：上游返回的是普通 error，我们只能靠 errors.Is 判断哨兵错误，
// 判断不了的一律当可重试。真正严谨的做法是让 train 包用自定义错误类型
// 显式标注"永久失败"，这也正是它该有的演进方向——ErrBrowserBusy 就是往这个方向
// 新加的一个。（注意：反过来不成立——不是所有"起不来浏览器"都归它，
// 例如浏览器在受限环境里压根活不下来仍算临时故障，train 包只对**有占用证据**的那种套哨兵。）
func retryable(err error, ctxErr error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, train.ErrManualLoginRequired):
		return false
	case errors.Is(err, train.ErrBrowserBusy):
		return false
	case ctxErr != nil:
		// 是我们取消了这次执行：超时、收到停止信号、或锁已丢失
		return false
	default:
		return true
	}
}

// maskUsername 给日志里的学号脱敏。
//
// 日志会被收集、转发、长期保存，还可能被贴进 issue 里求助。
// 学号是能定位到具体个人的标识，没有必要完整地留在日志中——
// 排查问题只需要能区分"是哪一次任务"，不需要知道是谁。
func maskUsername(s string) string {
	r := []rune(s)
	if len(r) <= 4 {
		return "****"
	}
	return string(r[:4]) + "****"
}

// sleep 睡一会儿，被取消时返回 false。
func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
