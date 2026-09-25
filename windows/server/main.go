package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"cqupt/ai"
	"cqupt/config"

	"github.com/joho/godotenv"
)

// ============================================================================
// main.go —— 服务入口：把 store / worker / HTTP 三层装起来，并处理优雅退出
//
// 这个进程里有两种生命周期完全不同的东西，装的时候必须分清：
//
//   - HTTP 层：请求是**短**的，收到停止信号应当立刻停止接受新连接，
//     再把手上这几个请求处理完（秒级）。
//   - worker：任务是**长**的，一次刷题要几分钟。收到停止信号应当停止领新任务，
//     但让手上的跑完（见 worker.Run 的注释）。
//
// 所以退出顺序是：先关 HTTP（不再接单）→ 再停 worker（把这一单做完）→ 最后退进程。
// 反过来的话，进程退出时可能还有请求正在写数据库。
//
// 和 CLI 入口（根目录 main.go）的分工：CLI 是给一个人在终端里手动刷一次题用的，
// 可以交互式提问、可以等人手动过验证码；这个入口是无人值守的，
// 配置只能来自环境变量，缺了就直接起不来——服务端没有任何人可问。
// ============================================================================

func main() {
	if err := run(); err != nil {
		// 这里不能用 slog：logger 可能都还没装配好。直接写 stderr。
		fmt.Fprintf(os.Stderr, "服务退出：%v\n", err)
		os.Exit(1)
	}
}

// run 把所有逻辑收进一个函数，好处是 defer 能正常生效——
// main 里直接 os.Exit 会跳过所有 defer，store 就关不上了。
func run() error {
	// 配置文件读不了就直接退出：格式坏的配置被静默跳过，比读不到更难查。
	if err := loadEnvFile(); err != nil {
		return err
	}

	// 站点配置（题目标题/选择器/各类超时/重答阈值）必须先加载。
	//
	// CLI 是在自己的 main 里调它的，服务端也一样得调——否则 config.C 全是零值，
	// 选择器是空串、超时是 0，worker 一跑就是"找不到元素"或"立刻超时"。
	// 这类问题的表现很像"站点改版了"，很容易查错方向。
	siteCfg := config.Load()

	log := setupLogger(siteCfg)
	slog.SetDefault(log)

	cfg, err := LoadConfig(os.Getenv)
	if err != nil {
		return err
	}
	// 只打印脱敏摘要。密钥和数据库口令会随日志被收集、转发、长期保存，
	// 打印它们等于把凭据又泄漏了一遍。
	log.Info("配置加载完成", "配置", cfg.Redacted())

	// 默认口令是写在 schema.sql 里的开发便利值，它就明晃晃地躺在仓库里。
	// 本机开发时无所谓（只监听 127.0.0.1、账号只有 DML 权限），
	// 但只要这个服务要放到别处跑，它就是一个公开的数据库口令。
	// 这里不做拒绝启动——那会打断本地开发的正常流程——但必须让人看见。
	if cfg.DBPassword == defaultDevDBPassword {
		log.Warn("正在使用 schema.sql 里的默认数据库口令，仅适合本机开发；"+
			"若部署到其他机器，请先改口令并同步更新 DB_PASSWORD",
			"账号", cfg.DBUser)
	}

	// Prompt 与模型句柄同样是 CLI 在 main 里初始化的，服务端不能省。
	// 顺序不能反：InitPrompts 决定要用哪些提示词，InitAI 才建模型客户端。
	ai.InitPrompts(siteCfg.PromptsFile)
	if err := ai.InitAI(); err != nil {
		// 起不来比带病运行好：模型调不通的话，每一个任务都会在答题那一步失败，
		// 而失败会被记进任务表，看起来像"站点出问题了"。
		return fmt.Errorf("初始化 AI 失败：%w", err)
	}

	store, err := OpenStore(cfg.DSN(), log)
	if err != nil {
		return err
	}
	defer store.Close()

	// 启动时就碰一次数据库。
	//
	// 不做这一步的话，数据库连不上时服务照样能起来、/healthz 照样返回 ok、
	// 负载均衡器照样往里送流量，然后每一个请求都失败。宁可起不来，
	// 让编排系统（或人）当场看见问题，也别让它带着一个必然失败的姿势上线。
	pingCtx, cancelPing := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelPing()
	if err := store.DB().PingContext(pingCtx); err != nil {
		return fmt.Errorf("连不上数据库（%s:%s/%s）：%w", cfg.DBHost, cfg.DBPort, cfg.DBName, err)
	}
	log.Info("数据库连接正常")

	// 收到 SIGINT（Ctrl+C）或 SIGTERM（docker stop / kill）时 cancel。
	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()

	// ---- worker ----
	worker := NewWorker(store, cfg, log)
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		worker.Run(ctx)
	}()

	// ---- HTTP ----
	srv := &http.Server{
		Addr:    cfg.Addr,
		Handler: NewServer(store, cfg, log).Handler(),

		// 这四个超时不是"优化"，是防资源耗尽：
		// 一个只连不发的客户端能占住一条连接和一份 goroutine，永不释放。
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       90 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		log.Info("HTTP 服务启动", "监听", cfg.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	// 谁先来就听谁的：要么是信号，要么是监听失败（端口被占）。
	select {
	case err := <-serveErr:
		if err != nil {
			// 端口被占用是最常见的启动失败原因，直接说出来，
			// 不要让用户从 "address already in use" 自己去推断。
			return fmt.Errorf("HTTP 监听 %s 失败：%w", cfg.Addr, err)
		}
		return nil
	case <-ctx.Done():
		log.Info("收到停止信号，开始优雅退出", "顺序", "先停 HTTP 接新请求，再等 worker 把手上的任务做完")
	}

	// 第一步：停止接受新连接，并把正在处理的请求处理完（它们都是毫秒级的）。
	// 必须用 context.WithoutCancel：ctx 此刻已经被信号取消，直接拿它当超时父级的话
	// Shutdown 会立刻返回，等于没等。
	shutdownCtx, cancelShutdown := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancelShutdown()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Warn("HTTP 优雅关闭超时，强制退出", "err", err)
	} else {
		log.Info("HTTP 已停止接受新请求，存量请求已处理完")
	}

	// 第二步：停 worker。注意 Run 是排空的——它会把手上的任务跑完才返回，
	// 所以这里要等，但不能无限等：万一任务卡在某个莫名其妙的地方，
	// 总得有个时间点让进程能退出去。
	grace := envDuration(os.Getenv, "SHUTDOWN_GRACE", 30*time.Second)
	select {
	case <-workerDone:
		log.Info("worker 已退出，本次退出没有留下未收尾的任务")
	case <-time.After(grace):
		log.Warn("等待 worker 超时，强制退出；"+
			"被中断的任务会停在 running，下次启动由锁超时回收重跑",
			"等待时长", grace)
	}

	log.Info("服务已退出")
	return nil
}

// loadEnvFile 依次尝试读几个约定好的环境文件。
//
// 为什么不像 CLI 那样直接读 .env：服务端配置和刷题配置是两拨人两件事，
// 混在同一个文件里，改一个的时候很容易顺手改坏另一个。
// 这里只是给本地开发提供便利，生产环境应当由部署系统注入环境变量。
//
// **必须区分「文件不存在」和「文件存在但解析失败」**。旧实现把 godotenv.Load
// 的任何错误都当成"换下一个候选试试"，于是只要 server.env 格式有问题，
// 它就会被**整个跳过**，最后表现为"我明明写了 TASK_SECRET，程序却说必填"，
// 而日志里一个字都不提——又是一次不报错的失败。
//
// 最典型的触发方式在 Windows 上：用 PowerShell 5.1 的 `Out-File -Encoding utf8`
// 生成配置文件会写进 UTF-8 BOM，godotenv 解析首行时直接报
// `unexpected character "»" in variable name`。（已实测确认。）
func loadEnvFile() error {
	for _, path := range []string{"server.env", ".env.server", ".env"} {
		if _, err := os.Stat(path); err != nil {
			continue // 不存在就是没配置，换下一个候选
		}
		if err := godotenv.Load(path); err != nil {
			return fmt.Errorf("读取配置文件 %s 失败：%w"+
				"（请检查文件编码与格式；常见原因是文件带 UTF-8 BOM——"+
				"Windows 上别用 PowerShell 5.1 的 Out-File -Encoding utf8 生成，改用「UTF-8 无 BOM」保存）",
				path, err)
		}
		return nil // Load 不覆盖已存在的环境变量，所以先找到的优先
	}
	return nil
}

// setupLogger 装配日志。
//
//   - 本地开发用 text：一行一条，人能直接读；
//   - 线上用 json：日志系统要的是机器可解析的字段，而不是靠正则去切文本。
//
// 格式不自己读环境变量，而是复用 config 包已经解析好的结果：
// LOG_FORMAT 只该有一个解释权，两个地方各读一次早晚会不一致。
//
// 加 source=true 是为了出问题时能直接看到"是哪一行代码打的这条日志"——
// 排查线上问题时，少一次翻代码就少一次猜。
func setupLogger(siteCfg config.Config) *slog.Logger {
	level := slog.LevelInfo
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LOG_LEVEL"))) {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	opts := &slog.HandlerOptions{Level: level, AddSource: true}

	if siteCfg.IsJSONLog() {
		return slog.New(slog.NewJSONHandler(os.Stdout, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stdout, opts))
}
