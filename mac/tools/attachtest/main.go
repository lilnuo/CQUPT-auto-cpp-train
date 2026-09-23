// attachtest 验证"先让浏览器无附加状态过 WAF 挑战、再附加 chromedp"的可行性：
//  1. 直接 exec 启动 Chrome（带 --remote-debugging-port，但不建立任何 CDP 会话），
//     并把登录 URL 作为启动参数——此时导航是"原生"的，瑞数 WAF 看不到任何自动化副作用；
//  2. 等 15 秒（挑战自动通过、页面加载完成）；
//  3. 用 chromedp NewRemoteAllocator 附加到该浏览器，检查页面是否有真实内容。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

const (
	loginURL = "https://prg.cqupt.edu.cn/indexcs/simple.jsp?loginErr=0"
	port     = "9223"
	profile  = ".chrome-profile-attachtest"
)

func trunc(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// findChrome 按常见顺序找 Chrome 可执行文件
func findChrome() (string, error) {
	candidates := []string{
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Applications/Chromium.app/Contents/MacOS/Chromium",
		"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("未找到 Chrome/Chromium/Edge，请安装或设置 CHROME_PATH")
}

func main() {
	chromeBin, err := findChrome()
	if err != nil {
		fmt.Println("!!", err)
		os.Exit(1)
	}
	fmt.Println(">> Chrome:", chromeBin)

	// 1) 原生启动 Chrome：带远程调试端口 + 独立 profile + 直接打开登录页。
	//    注意：此时没有任何 CDP 客户端附加，页面加载过程对 WAF 而言与真人无异。
	cmd := exec.Command(chromeBin,
		"--remote-debugging-port="+port,
		"--user-data-dir="+profile,
		"--no-first-run",
		"--no-default-browser-check",
		loginURL,
	)
	if err := cmd.Start(); err != nil {
		fmt.Println("!! 启动 Chrome 失败:", err)
		os.Exit(1)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	fmt.Println(">> Chrome 已原生启动（无 CDP 附加），等待 WAF 挑战自动通过...")

	// 2) 等调试端口就绪（/json/version 可访问即说明浏览器起来了）
	ready := false
	for i := 0; i < 20; i++ {
		resp, err := http.Get("http://127.0.0.1:" + port + "/json/version")
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			ready = true
			break
		}
		time.Sleep(1 * time.Second)
	}
	if !ready {
		fmt.Println("!! 调试端口 15s 内未就绪")
		os.Exit(1)
	}
	fmt.Println(">> 调试端口就绪，再给挑战 12 秒完成时间...")
	time.Sleep(12 * time.Second)

	// 2.5) 建 allocator（远程附加模式）
	alloctx, cancelAlloc := chromedp.NewRemoteAllocator(context.Background(), "http://127.0.0.1:"+port)
	defer cancelAlloc()

	// 3) 附加 chromedp：注意 NewContext 默认会新建空白标签页，
	//    必须先通过 HTTP /json/list 枚举 targets、找到已打开登录页的那个，再用 WithTargetID 接管。
	listResp, err := http.Get("http://127.0.0.1:" + port + "/json/list")
	if err != nil {
		fmt.Println("!! /json/list 请求失败:", err)
		os.Exit(1)
	}
	var tabs []struct {
		ID   string `json:"id"`
		Type string `json:"type"`
		URL  string `json:"url"`
	}
	if err := json.NewDecoder(listResp.Body).Decode(&tabs); err != nil {
		fmt.Println("!! 解析 /json/list 失败:", err)
		os.Exit(1)
	}
	listResp.Body.Close()
	var targetID target.ID
	fmt.Println(">> 当前 targets:")
	for _, tab := range tabs {
		fmt.Printf("   type=%s url=%s\n", tab.Type, trunc(tab.URL, 90))
		if tab.Type == "page" && contains(tab.URL, "prg.cqupt.edu.cn") {
			targetID = target.ID(tab.ID)
		}
	}
	if targetID == "" {
		fmt.Println("!! 没找到登录页标签页（可能 Chrome 启动时没打开 URL？）")
		os.Exit(1)
	}
	fmt.Println(">> 接管已存在的登录页标签页:", targetID)

	ctx, cancel := chromedp.NewContext(alloctx, chromedp.WithTargetID(targetID))
	defer cancel()

	runCtx, cancelRun := context.WithTimeout(ctx, 30*time.Second)
	defer cancelRun()

	var htmlLen int
	var t, h string
	_ = chromedp.Run(runCtx, chromedp.Evaluate(`document.title`, &t))
	_ = chromedp.Run(runCtx, chromedp.Evaluate(`location.href`, &h))
	err = chromedp.Run(runCtx, chromedp.Evaluate(`document.documentElement ? document.documentElement.outerHTML.length : -1`, &htmlLen))
	if err != nil {
		fmt.Println("!! Evaluate 失败:", err)
		os.Exit(1)
	}
	fmt.Printf(">> 附加后页面: title=%q len=%d href=%s\n", t, htmlLen, h)

	if htmlLen > 2000 {
		fmt.Println(">>> 成功！挑战已过、页面有真实内容。保存 page.html / 截图 / 表单摘要...")
		var html string
		_ = chromedp.Run(runCtx, chromedp.Evaluate(`document.documentElement.outerHTML`, &html))
		os.WriteFile("page.html", []byte(html), 0644)
		fmt.Printf(">>> page.html 已保存（%d 字节）\n", len(html))

		var summary string
		_ = chromedp.Run(runCtx, chromedp.Evaluate(`JSON.stringify((function(){
			return {
				inputs: Array.from(document.querySelectorAll('input')).map(function(el){return {type:el.type||'',name:el.name||'',id:el.id||'',ph:el.placeholder||''};}),
				imgs: Array.from(document.querySelectorAll('img')).map(function(el){return {src:(el.src||'').slice(0,100),id:el.id||'',w:el.width,h:el.height};}),
				iframes: Array.from(document.querySelectorAll('iframe')).map(function(el){return {src:(el.src||'').slice(0,100)};})
			};
		})())`, &summary))
		fmt.Println(">>> 表单摘要:", summary)

		// ===== 阶段2：附加后再导航同一 URL，验证后续导航是否会被 WAF 再次拦截 =====
		fmt.Println(">> [阶段2] 附加后再 Navigate 同一 URL，测试后续导航...")
		navCtx, cancelNav := context.WithTimeout(ctx, 40*time.Second)
		defer cancelNav()
		if err := chromedp.Run(navCtx, chromedp.Navigate(loginURL)); err != nil {
			fmt.Println("!! 再导航失败:", err)
		} else {
			time.Sleep(8 * time.Second)
			var l2 int
			var t2 string
			_ = chromedp.Run(navCtx, chromedp.Evaluate(`document.title`, &t2))
			_ = chromedp.Run(navCtx, chromedp.Evaluate(`document.documentElement ? document.documentElement.outerHTML.length : -1`, &l2))
			fmt.Printf(">> [阶段2] 再导航后: title=%q len=%d %s\n", t2, l2, boolMark(l2 > 2000, "内容正常，后续导航可用", "仍被拦截！"))
		}

		// ===== 阶段3：元素截图方式抓验证码图片，存成 captcha.png 供 OCR 测试 =====
		fmt.Println(">> [阶段3] 截图验证码图片...")
		var capPng []byte
		if err := chromedp.Run(navCtx, chromedp.Screenshot(`img[src="/cgjiaoyan"]`, &capPng, chromedp.NodeVisible, chromedp.ByQuery)); err != nil {
			fmt.Println("!! 截验证码失败:", err)
		} else {
			os.WriteFile("captcha.png", capPng, 0644)
			fmt.Printf(">> [阶段3] 验证码已存 captcha.png（%d 字节）\n", len(capPng))
		}
	} else {
		fmt.Println(">>> 失败：附加后页面仍是空白，说明 WAF 在附加后立即再次拦截")
	}
}

func boolMark(ok bool, yes, no string) string {
	if ok {
		return yes
	}
	return no
}
