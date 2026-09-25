// Command cdpdiag 逐层复刻主流程的浏览器建立步骤，用来定位
// 「建立浏览器控制连接失败」到底卡在哪一层。
//
// 主流程里这段失败时只会抛出一句包装后的错误（例如 "context canceled"），
// 看不出是哪一步的问题。这个工具把每一步单独打出来：
//
//  1. 启动 Chrome（参数与主流程一致）并读 stderr 抓 DevTools 地址
//  2. 用原生 HTTP 打 /json/version 与 /json/list，确认端口真的在服务
//  3. 建 RemoteAllocator + Context，跑一次**空 actions** 的 chromedp.Run（主流程当前做法）
//  4. 再跑一次带真实 action 的 Run，对比两者
//
// 用法：
//
//	go run ./tools/cdpdiag -headless             # 推荐：不开窗口，纯查连接层
//	go run ./tools/cdpdiag                      # ⚠️ 会真的弹出一个 Chrome 窗口
//	go run ./tools/cdpdiag -port 9333 -temp-profile   # 换端口 + 临时 profile
//	go run ./tools/cdpdiag -keep                      # 不杀 Chrome，留给人工检查
//
// ⚠️ 这个工具会**真的启动 Chrome**，结束时再把它杀掉（除非 -keep）——
// 也就是说屏幕上会有一个窗口一开一关。想避免这件事就用 -headless。
// 需要查的是 WAF 挑战、登录页这些"必须有窗口"的环节时，才用不带 -headless 的模式。
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"cqupt/config"

	"github.com/chromedp/chromedp"
)

func main() {
	port := flag.String("port", config.DefaultCDPPort, "远程调试端口")
	tempProfile := flag.Bool("temp-profile", false, "用临时 profile（排除 profile 锁与残留状态的干扰）")
	keep := flag.Bool("keep", false, "结束时保留 Chrome 进程，方便人工检查")
	headless := flag.Bool("headless", false, "以无窗口模式启动 Chrome（不弹窗口，适合只查连接层）")
	flag.Parse()

	config.Load()

	profile := config.UserDataDir()
	if *tempProfile {
		dir, err := os.MkdirTemp("", "cdpdiag-profile-")
		if err != nil {
			fmt.Println("创建临时 profile 失败:", err)
			os.Exit(1)
		}
		defer os.RemoveAll(dir)
		profile = dir
	}

	// 先讲清楚它要干什么。屏幕上突然冒出一个 Chrome 又消失，
	// 如果没人告诉你原因，看着就像程序在发疯。
	if !*headless && !*keep {
		fmt.Println("⚠️  即将启动一个 Chrome 窗口，并在几秒后把它关掉（这是诊断步骤，不是异常）。")
		fmt.Println("    想避免弹窗请加 -headless。")
	}
	fmt.Printf("=== 环境 ===\n端口=%s\nprofile=%s\n登录页=%s\n无窗口模式=%v\n\n",
		*port, profile, config.URLLogin, *headless)

	// ---------- 第 1 步：启动 Chrome 并抓 DevTools 地址 ----------
	cmd, ws, err := launchChrome(profile, *port, *headless)
	if err != nil {
		fmt.Println("❌ 第 1 步 启动 Chrome 失败:", err)
		os.Exit(1)
	}
	// 顺手记下 Chrome 自己的退出码/信号：它"启动后没多久就死了"是
	// 这类问题的关键证据，光看连接层的报错是看不出来的。
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	if !*keep {
		defer func() {
			_ = cmd.Process.Kill()
			select {
			case <-waitCh:
			case <-time.After(3 * time.Second):
			}
		}()
	}
	if ws == "" {
		fmt.Printf("⚠️  第 1 步 进程起来了，但 %s 内没在 stderr 读到 DevTools 地址\n"+
			"    → 主流程此时会退回 http://127.0.0.1:%s\n\n", config.TimeoutChromeWSWait, *port)
	} else {
		fmt.Printf("✅ 第 1 步 抓到 DevTools 地址：%s\n\n", ws)
	}

	// ---------- 第 1.5 步：Chrome 自己还活着吗 ----------
	// 这一步是判断"连接层报错"背后真相的关键：如果 Chrome 进程已经退出，
	// 那连接失败只是**结果**，真正的问题是浏览器没能活着起来。
	// 只看 chromedp 抛出的那句 "context canceled"，永远看不出这一点。
	select {
	case werr := <-waitCh:
		fmt.Printf("❌ 第 1.5 步 Chrome 进程已经退出，Wait 返回: %v\n", werr)
		fmt.Println("    → 连接失败是**结果**而不是原因：真正的问题是浏览器没能活着起来。")
		fmt.Println("    → 常见于受限的运行环境（沙箱、无 GUI 权限）或安全软件拦截。")
	case <-time.After(300 * time.Millisecond):
		fmt.Println("✅ 第 1.5 步 Chrome 进程仍在运行")
	}
	fmt.Println()

	// ---------- 第 2 步：原生 HTTP 探端点（不经 chromedp） ----------
	base := "http://127.0.0.1:" + *port
	for _, path := range []string{"/json/version", "/json/list"} {
		body, code, err := httpGet(base + path)
		if err != nil {
			fmt.Printf("❌ 第 2 步 GET %s 失败: %v\n", path, err)
			continue
		}
		fmt.Printf("✅ 第 2 步 GET %s → HTTP %d\n", path, code)
		if path == "/json/list" {
			var items []map[string]any
			if json.Unmarshal([]byte(body), &items) == nil {
				fmt.Printf("   当前 target 数 = %d\n", len(items))
				for i, it := range items {
					fmt.Printf("     [%d] type=%v title=%q\n", i, it["type"], it["title"])
				}
			}
		}
	}
	fmt.Println()

	// ---------- 第 3 步：空 actions 的 Run（主流程当前就是这么做的） ----------
	allocURL := ws
	if allocURL == "" {
		allocURL = base
	}
	alloctx, cancelAlloc := chromedp.NewRemoteAllocator(context.Background(), allocURL)
	defer cancelAlloc()
	ctx, cancelCtx := chromedp.NewContext(alloctx)
	defer cancelCtx()

	start := time.Now()
	errEmpty := chromedp.Run(ctx)
	fmt.Printf("=== 第 3 步 空 actions 的 chromedp.Run（耗时 %s）===\n", time.Since(start).Round(time.Millisecond))
	if errEmpty != nil {
		fmt.Printf("❌ 失败: %v\n\n", errEmpty)
	} else {
		fmt.Println("✅ 成功")
	}

	// ---------- 第 4 步：带真实 action 的 Run ----------
	start = time.Now()
	var title string
	errReal := chromedp.Run(ctx, chromedp.Evaluate("document.title", &title))
	fmt.Printf("=== 第 4 步 带真实 action 的 chromedp.Run（耗时 %s）===\n", time.Since(start).Round(time.Millisecond))
	if errReal != nil {
		fmt.Printf("❌ 失败: %v\n", errReal)
	} else {
		fmt.Printf("✅ 成功，当前页标题 = %q\n", title)
	}

	// ---------- 结论 ----------
	chromeStillHere := true
	select {
	case <-waitCh:
		chromeStillHere = false
	default:
	}

	fmt.Println("\n=== 结论 ===")
	if !chromeStillHere {
		fmt.Println("Chrome 进程已经死了。这时连接层的报错（常见是「context canceled」）" +
			"只是**结果**，不是原因——别去调连接参数，先解决「浏览器为什么活不下来」。" +
			"换到普通终端里跑、或检查是否有安全软件拦截。")
		return
	}
	switch {
	case errEmpty == nil && errReal == nil:
		fmt.Println("两层都通过 → 浏览器建立这一段在本机是好的。若主流程仍失败，" +
			"问题在更后面的步骤（等 WAF 挑战、进登录页）。")
	case errEmpty != nil && errReal == nil:
		fmt.Println("空 actions 失败、真实 action 成功 → 失败出在「用空 Run 建连接」这一步，" +
			"应改成用真实 action 建连接。")
	case errEmpty == nil && errReal != nil:
		fmt.Println("空 Run 通过但真实 action 失败 → 连接建起来了但 target 不可用" +
			"（常见原因：profile 有残留状态、或页面被 WAF 拦成空壳）。")
	default:
		fmt.Println("两层都失败，但 Chrome 还活着 → 是「连不上 Chrome」这一类问题，" +
			"看上面第 1、2 步的输出来定位（重点看第 1 步有没有抓到 DevTools 地址）。")
	}
}

// launchChrome 的启动参数与读 stderr 逻辑跟 train 包里的私有实现保持一致。
//
// 刻意复制一份而不是导出复用：那是生产代码的私有细节，
// 而诊断工具要额外把「读到了什么、花了多久」打出来，语义并不相同。
func launchChrome(profile, port string, headless bool) (*exec.Cmd, string, error) {
	bin, err := config.FindChrome()
	if err != nil {
		return nil, "", err
	}
	args := []string{
		"--remote-debugging-port=" + port,
		"--user-data-dir=" + profile,
		"--no-first-run",
		"--no-default-browser-check",
	}
	if headless {
		args = append(args, "--headless=new")
	}
	args = append(args, config.URLLogin)
	cmd := exec.Command(bin, args...)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, "", fmt.Errorf("获取 Chrome stderr 失败: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, "", fmt.Errorf("启动 Chrome 失败: %w", err)
	}

	wsCh := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stderr)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		const marker = "DevTools listening on "
		for scanner.Scan() {
			line := scanner.Text()
			if idx := strings.Index(line, marker); idx >= 0 {
				select {
				case wsCh <- strings.TrimSpace(line[idx+len(marker):]):
				default:
				}
			}
		}
	}()

	select {
	case ws := <-wsCh:
		return cmd, ws, nil
	case <-time.After(config.TimeoutChromeWSWait):
		return cmd, "", nil
	}
}

func httpGet(url string) (string, int, error) {
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return string(b), resp.StatusCode, err
}
