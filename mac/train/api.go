// Package train 承载「刷一次题」这件事本身。
//
// 这一层被两条入口共用：
//
//   - 命令行入口（仓库根目录的 main.go）——交互式，允许人工兜底登录；
//   - HTTP 服务入口（server/）——无人值守，任务由 worker 从队列里领出来执行。
//
// 为什么必须单独成包：**Go 不允许别的包导入 package main**。想让服务端复用刷题流程，
// 流程就得先离开 main 包。这本身也是服务化的第一道坎——「能跑起来的脚本」和
// 「能被别的代码调用的模块」是两回事：脚本把输入读自 stdin、把结果打到终端、
// 用退出码表示成败；模块则必须把输入、输出、错误三样都变成显式的参数与返回值。
package train

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// 运行模式。
const (
	ModeQuiz       = "quiz"       // 整页选择/填空题（默认）
	ModeProgap     = "progap"     // 程序片段编程题
	ModeProgapDump = "progapdump" // 只 dump 第一道程序题页面，用于调试选择器
)

// Request 描述一次刷题任务。
type Request struct {
	Username string
	Password string
	Num      int    // 打算刷多少题
	Mode     string // 见 Mode* 常量；留空按 ModeQuiz 处理
	Dump     bool   // quiz 模式下把答题页 dump 到 page.html
}

// Normalize 补齐缺省值并返回副本，不改动调用方的原始值。
func (r Request) Normalize() Request {
	if r.Mode == "" {
		r.Mode = ModeQuiz
	}
	return r
}

// Validate 检查必填项。
//
// 服务端应当在**落库之前**就调用它：一个参数就不合法的任务没有任何理由进队列，
// 更不该等 worker 领出来、起了浏览器、跑了二十秒才失败。
func (r Request) Validate() error {
	if r.Username == "" {
		return errors.New("学号不能为空")
	}
	if r.Password == "" {
		return errors.New("密码不能为空")
	}
	if r.Num <= 0 {
		return fmt.Errorf("题量必须是正整数，收到 %d", r.Num)
	}
	switch r.Mode {
	case ModeQuiz, ModeProgap, ModeProgapDump:
		return nil
	default:
		return fmt.Errorf("未知模式 %q（可选：%s / %s / %s）",
			r.Mode, ModeQuiz, ModeProgap, ModeProgapDump)
	}
}

// IsProgap 报告该模式是否走程序片段编程题流程。
func (r Request) IsProgap() bool {
	return r.Mode == ModeProgap || r.Mode == ModeProgapDump
}

// 进度事件的类型。
const (
	EventStarted      = "started"       // 任务开始
	EventLoginOK      = "login_ok"      // 登录成功
	EventAssignmentIn = "assignment_in" // 已进入答题页
	EventQuestionDone = "question_done" // 某道题已提交并读到得分
	EventManualLogin  = "manual_login"  // 需要人工登录（服务端会直接判失败）
	EventFinished     = "finished"      // 任务结束
)

// Event 是任务执行过程中的一个进度事件。
//
// 服务端把它逐条落库，于是「任务跑到哪一步了」这个问题有了真实答案——
// 而不是靠解析子进程的标准输出猜。
type Event struct {
	Kind   string  // 见 Event* 常量
	Detail string  // 人类可读的补充说明
	Pid    string  // 相关题目标识，可为空
	Score  float64 // 相关得分，可为 0
	At     time.Time
}

// QuestionResult 是一道题的作答结果。
//
// 注意这里收集的是**每一道做过的题**，不只是做错的题。
// 只记错题会让"这次一共做了几道、对了几道、总共拿了多少分"变成算不出来的数字——
// 而这三件事恰恰是复盘和统计最先要回答的问题。
type QuestionResult struct {
	Pid      string  // 题目标识
	Text     string  // 题干
	Answer   string  // 最后一次给出的答案
	Score    float64 // 该题得分；-1 表示无法判定（页面没有总分区域）
	Tries    int     // 实际作答次数
	Judgment string  // 判定说明（程序题的回显结论等）
}

// Passed 报告这道题是否拿到了分。
// 得分无法判定（-1）时按通过处理——拿不准的事情不该触发重跑。
func (q QuestionResult) Passed() bool { return q.Score != 0 }

// Result 是一次任务的产出。
type Result struct {
	Score     int              // 最终总分
	Mode      string           // 实际执行的模式
	Questions []QuestionResult // 每道做过的题
}

// Failed 返回没有拿到分的题，用于写错题本。
func (r Result) Failed() []QuestionResult {
	var out []QuestionResult
	for _, q := range r.Questions {
		if !q.Passed() {
			out = append(out, q)
		}
	}
	return out
}

// Options 控制这一次运行与外部世界的交互方式。
type Options struct {
	// AllowManualLogin 决定验证码识别失败时是否停下来等人手动登录。
	//
	// 命令行下为 true。**服务端必须为 false**：无人值守的环境里「等人」
	// 等于永久挂起，会把唯一的 worker 占死、后面所有任务全部堵住。
	// 这是「交互式脚本」与「无人值守服务」最本质的一处差别。
	AllowManualLogin bool

	// OnEvent 是进度回调，可为 nil。
	// 它在任务所在的 goroutine 上**同步**调用，实现方自己保证不阻塞。
	OnEvent func(Event)

	// Logger 是本次运行的日志器，为 nil 时沿用当前的默认 logger。
	//
	// 服务端传入带 task_id 的 logger，运行期间它被设为进程默认 logger，
	// 于是流程内部散落各处的 slog 调用会自动带上任务标识，
	// 多任务串行跑的时候不至于分不清哪行日志属于哪个任务。
	Logger *slog.Logger
}

// 包级运行态。
//
// 这里用包级变量、而不是把 Options 层层穿过二十多个函数，是因为这个流程
// 本身就不可能并发：它要独占一个 Chrome 进程、独占一个 profile 目录
// （Chrome 对同一 profile 有单实例锁），还要占用固定的 CDP 调试端口。
// runMu 把这件事实直接钉死在代码里——同一进程内，刷题只能一个接一个地跑。
var (
	runMu   sync.Mutex
	curOpts Options
	curLog  *slog.Logger

	// curResults 收集本次运行每道题的结果。
	curResults []QuestionResult
)

// ErrManualLoginRequired 在服务端遇到"需要人工登录"时返回。
//
// 导出它是有意的：调用方要能区分"这次运气不好"和"这事重试也没用"。
// 服务端的重试策略就靠 errors.Is(err, ErrManualLoginRequired) 判断
// 该不该把任务退回队列——见 server/worker.go 的 retryable。
var ErrManualLoginRequired = errors.New(
	"验证码识别失败，且服务端不允许人工登录（无人值守任务无法等待人工介入）")

// ErrBrowserBusy 在"本机浏览器调试资源已被另一个 Chrome 占用"时返回。
//
// 同样导出，理由也一样：这是**确定性的本地环境问题**，重试不会改变任何东西，
// 只会再等一轮超时、再在用户屏幕上弹一次窗口。服务端据此直接判终态，
// 而不是把 2 次重试配额白白花掉。
//
// 具体的证据（哪个端口被谁占了、profile 被哪个 PID 持有）由调用处拼进错误文本，
// 哨兵本身只表达"这一类问题"。
var ErrBrowserBusy = errors.New("浏览器调试资源已被占用")

// Run 执行一次刷题任务，阻塞直到结束。
//
// 它是串行的：runMu 保证同一时刻只有一个任务在真正操作浏览器。
// 这不是偷懒，而是被浏览器的物理约束逼出来的（见上面 runMu 的注释）。
// 也正因为如此，服务端必须有一层队列——否则并发请求会直接在这里排长队，
// 请求方既看不到进度、也不知道自己排在第几位。
func Run(ctx context.Context, req Request, opt Options) (Result, error) {
	runMu.Lock()
	defer runMu.Unlock()

	req = req.Normalize()
	if err := req.Validate(); err != nil {
		return Result{Mode: req.Mode}, err
	}

	// 装好本次运行的上下文，结束前恢复原样，避免影响下一次运行
	curOpts = opt
	curResults = nil
	prevLogger := slog.Default()
	if opt.Logger != nil {
		slog.SetDefault(opt.Logger)
	}
	defer func() {
		slog.SetDefault(prevLogger)
		curOpts = Options{}
		curLog = nil
	}()

	emit(Event{Kind: EventStarted, Detail: "模式 " + req.Mode})

	var (
		score int
		err   error
	)
	if req.IsProgap() {
		score, err = runProgap(req.Username, req.Password, req.Num, req.Mode == ModeProgapDump)
	} else {
		score, err = runQuiz(req.Username, req.Password, req.Num, req.Dump)
	}
	if err != nil {
		return Result{Mode: req.Mode, Questions: curResults}, err
	}

	emit(Event{Kind: EventFinished, Detail: fmt.Sprintf("总分 %d", score), Score: float64(score)})
	return Result{Score: score, Mode: req.Mode, Questions: curResults}, nil
}

// emit 上报一个进度事件。OnEvent 为 nil 时什么也不做。
func emit(e Event) {
	if e.At.IsZero() {
		e.At = time.Now()
	}
	if cb := curOpts.OnEvent; cb != nil {
		cb(e)
	}
}

// noteResult 记录一道题的作答结果，并把它作为一条进度事件上报。
//
// 把"记结果"和"报进度"合成一个入口，是为了避免两处各写一遍：
// 结果表和事件流本来就该描述同一件事，分别维护迟早会不一致——
// 进度页显示"做了 7 题"，结果表里却只有 6 行。
func noteResult(q QuestionResult) {
	curResults = append(curResults, q)

	detail := q.Judgment
	if detail == "" {
		detail = fmt.Sprintf("作答 %d 次，得 %g 分", q.Tries, q.Score)
	}
	emit(Event{Kind: EventQuestionDone, Pid: q.Pid, Score: q.Score, Detail: detail})
}
