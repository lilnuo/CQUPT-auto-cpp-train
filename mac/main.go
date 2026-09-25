// 命令行入口。
//
// 这一层只负责三件事：解析命令行参数、跟用户做交互式问答、把结果打到终端。
// 真正的刷题流程在 train 包里 —— 因为 Go 不允许别的包导入 package main，
// 想让 server/ 复用这套流程，它就必须待在 main 之外。
//
// 把它想成"外壳"：外壳管输入输出，内核管业务。将来加 Web 入口（server/）时，
// 换的只是外壳，内核一行不用动。
package main

import (
	"bufio"
	"context"
	"cqupt/ai"
	"cqupt/config"
	"cqupt/train"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// stdin 供交互式输入使用。注意 train 包内部也持有自己的 stdin reader，
// 但两者的读取时机是严格先后关系（main 先读完三个参数，train 才可能在
// 人工登录兜底时去读），不会交叉，所以不需要共享同一个 reader。
var stdin = bufio.NewReader(os.Stdin)

// modeFlag 运行模式：
//   - quiz（默认）  ：整页选择填空题
//   - progap        ：程序片段编程题
//   - progapdump    ：只 dump 第一道程序题页面，用于调试选择器
var modeFlag = flag.String("mode", train.ModeQuiz,
	"运行模式：quiz=整页选择填空（默认）；progap=程序片段编程题；progapdump=只 dump 程序题页面")

func main() {
	dump := flag.Bool("dump", false, "进入做题页后把页面 HTML 保存到 page.html，用于调试选择器")
	logJSON := flag.Bool("log-json", false, "以 JSON 格式输出日志（也可用环境变量 LOG_FORMAT=json）")
	promptsInit := flag.Bool("prompts-init", false, "生成 prompts.example.json 模板后退出，便于外部覆盖 Prompt")
	flag.Parse()

	// 配置集中加载（含 .env）：必须在任何 os.Getenv 之前完成
	config.Load()
	if *logJSON {
		config.C.LogFormat = "json"
	}
	setupLogger()

	if *promptsInit {
		if err := writePromptTemplate(); err != nil {
			slog.Error("生成 Prompt 模板失败", "err", err)
			os.Exit(1)
		}
		return
	}

	// Prompt 的加载与模型初始化解耦：dump 模式不调模型，但仍可能有流程用到 Prompt
	ai.InitPrompts(config.C.PromptsFile)

	if *dump {
		// dump 只抓取页面结构、不需要调用大模型，因此跳过 AI 初始化
		// （这样即使 .env 没填 ARK_API_KEY 也能跑 dump）
		slog.Info("[dump 模式] 跳过 AI 初始化，无需填写 ARK_API_KEY")
	} else {
		if err := ai.InitAI(); err != nil {
			slog.Error("AI 初始化失败", "err", err,
				"hint", "请检查 .env 中的 ARK_API_KEY 与 ARK_MODEL_ID")
			os.Exit(1)
		}
	}

	// 交互式收集三个参数。命令行入口允许人工兜底登录（浏览器可见、终端可回车），
	// 这是它区别于服务端入口的地方 —— 服务端必须无人值守，见 train.Options。
	req := train.Request{Mode: *modeFlag, Dump: *dump}
	fmt.Println("请输入你的学号")
	req.Username = readLine()
	fmt.Println("请输入你的密码")
	req.Password = readLine()
	fmt.Println("输入你想刷的题目数量")
	numStr := readLine()

	num, err := parseNum(numStr)
	if err != nil {
		slog.Error("题目数量必须是一个正整数", "输入", numStr)
		os.Exit(1)
	}
	req.Num = num

	// 交互模式下把进度事件打到终端，用户能实时看到"登录成功/进入答题页/第几题得了几分"
	res, err := train.Run(context.Background(), req, train.Options{
		AllowManualLogin: true,
		OnEvent:          printEvent,
	})
	if err != nil {
		slog.Error("刷题过程出错", "err", err)
		os.Exit(1)
	}
	fmt.Printf("最终得分为 %d 分\n", res.Score)
}

// parseNum 把用户输入的题量字符串转成正整数。
// 单独抽出来是为了能测：交互式输入的解析逻辑最容易在边界上出错
// （空输入、带空格、负数、超范围）。
func parseNum(s string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("不是合法数字: %q", s)
	}
	if n <= 0 {
		return 0, fmt.Errorf("必须是正整数，收到 %d", n)
	}
	return n, nil
}

// printEvent 把 train 上报的进度事件打到终端。
//
// 事件里不打印题干与答案 —— 它们可能很长，会把终端刷得看不清。
func printEvent(e train.Event) {
	switch e.Kind {
	case train.EventStarted:
		slog.Info("开始刷题", "详情", e.Detail)
	case train.EventLoginOK:
		slog.Info("登录成功")
	case train.EventAssignmentIn:
		slog.Info("已进入答题页，开始逐题作答")
	case train.EventQuestionDone:
		slog.Info("完成一题", "题", e.Pid, "得分", e.Score, "详情", e.Detail)
	case train.EventFinished:
		slog.Info("任务结束", "详情", e.Detail)
	}
}

// setupLogger 按配置初始化 slog。
// 文本格式下把时间压成 HH:MM:SS，避免默认的完整时间戳把日志挤得很长。
func setupLogger() {
	opts := &slog.HandlerOptions{
		Level: slog.LevelInfo,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				if t, ok := a.Value.Any().(time.Time); ok {
					a.Value = slog.StringValue(t.Format("15:04:05"))
				}
			}
			return a
		},
	}
	var h slog.Handler
	if config.C.IsJSONLog() {
		h = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		h = slog.NewTextHandler(os.Stdout, opts)
	}
	slog.SetDefault(slog.New(h))
}

// writePromptTemplate 生成一份可编辑的 Prompt 模板文件
func writePromptTemplate() error {
	b, err := ai.DumpPromptTemplate()
	if err != nil {
		return err
	}
	const out = "prompts.example.json"
	if err := os.WriteFile(out, b, 0644); err != nil {
		return err
	}
	fmt.Printf("已生成 %s，复制为 %s 并按需修改即可覆盖内置 Prompt\n", out, config.DefaultPromptsFile)
	return nil
}

// readLine 读取一行并去除首尾空白
func readLine() string {
	line, _ := stdin.ReadString('\n')
	return strings.TrimSpace(line)
}
