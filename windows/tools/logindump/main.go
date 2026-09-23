// logindump 是一个一次性调试工具：打开 CQUPT 练习系统登录页，
// 保存整页 HTML、整页截图，并尝试截图验证码图片，用于确认真实 DOM 结构。
// 用法：
//
//	go run ./tools/logindump            # 无头模式
//	go run ./tools/logindump -visible   # 可见模式（弹窗，用于绕过 WAF 对无头的拦截）
package main

import (
	"context"
	"encoding/base64"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// indexOf: strings.Index 的简单包装（避免与 flag 等混淆，这里直接用 strings 亦可）
func indexOf(s, sub string) int { return strings.Index(s, sub) }

// truncURL 截断超长 URL，方便日志阅读
func truncURL(s string) string {
	if len(s) > 110 {
		return s[:110] + "..."
	}
	return s
}

const loginURL = "https://prg.cqupt.edu.cn/indexcs/simple.jsp?loginErr=0"

// stealthJS 反检测补丁：在每个新文档加载前注入，抹掉自动化浏览器指纹，
// 绕过瑞数 WAF 的 navigator.webdriver 等检测。
const stealthJS = `(function(){
	// 1) 最核心：navigator.webdriver 恒为 false/undefined
	Object.defineProperty(navigator, 'webdriver', { get: function(){ return undefined; } });
	// 2) 补齐真实浏览器该有的 window.chrome
	window.chrome = window.chrome || { runtime: {} };
	// 3) 语言 / 插件数量（无头默认可能为空）
	Object.defineProperty(navigator, 'languages', { get: function(){ return ['zh-CN','zh','en']; } });
	Object.defineProperty(navigator, 'plugins', { get: function(){ return [1,2,3,4,5]; } });
	// 4) permissions.query 指纹补丁
	if (navigator.permissions && navigator.permissions.query) {
		var origQuery = navigator.permissions.query.bind(navigator.permissions);
		navigator.permissions.query = function(p){
			if (p && p.name === 'notifications') return Promise.resolve({ state: Notification.permission });
			return origQuery(p);
		};
	}
})()`

func main() {
	visible := flag.Bool("visible", false, "可见浏览器模式（默认无头）")
	stealth := flag.Bool("stealth", false, "注入反检测补丁")
	outDir := flag.String("out", ".", "输出目录")
	url := flag.String("url", loginURL, "要抓取的 URL（默认登录页）")
	profile := flag.String("profile", ".chrome-profile-logindump", "浏览器用户数据目录")
	flag.Parse()

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		// 独立的临时 profile，避免与主程序的 .chrome-profile 冲突
		chromedp.UserDataDir(*profile),
		// 注意：不要加 --disable-gpu！它会让 WebGL 指纹变成 SwiftShader 软件渲染，
		// 是无头/自动化浏览器的经典特征，瑞数 WAF 会据此拒绝渲染。
		// macOS 的 Chrome 也不需要 --no-sandbox。
		chromedp.Flag("disable-dev-shm-usage", true),
		// 反自动化检测：去掉 --enable-automation 开关（该开关会让 navigator.webdriver=true）
		chromedp.Flag("exclude-switches", "enable-automation"),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
	)
	if !*visible {
		opts = append(opts, chromedp.Flag("headless", true))
	}
	alloctx, cancelAlloc := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancelAlloc()

	ctx, cancel := chromedp.NewContext(alloctx)
	defer cancel()

	// 网络层诊断：打印每个响应的状态码，看 WAF 挑战重载后服务器返回什么
	chromedp.ListenTarget(ctx, func(ev interface{}) {
		if e, ok := ev.(*network.EventResponseReceived); ok {
			fmt.Printf("   [net] %d %s\n", e.Response.Status, truncURL(e.Response.URL))
		}
	})

	// 总超时 90s：导航 + WAF 挑战（瑞数会自刷新一次页面）+ 渲染
	runCtx, cancelRun := context.WithTimeout(ctx, 90*time.Second)
	defer cancelRun()

	fmt.Println(">> 正在打开登录页...", *url)
	// 可选：注入 stealth 补丁再导航，抹掉自动化指纹。
	actions := []chromedp.Action{}
	if *stealth {
		actions = append(actions, chromedp.ActionFunc(func(c context.Context) error {
			_, err := page.AddScriptToEvaluateOnNewDocument(stealthJS).Do(c)
			return err
		}))
	}
	// 先开启 network 事件监听，再导航
	actions = append(actions, network.Enable(), chromedp.Navigate(*url))
	err := chromedp.Run(runCtx, actions...)
	if err != nil {
		fmt.Printf("!! 导航失败: %v\n", err)
		os.Exit(1)
	}
	// 瑞数挑战会自刷新页面 1~2 次。轮询 30 秒观察加载过程：
	// 看 href/标题/HTML 长度怎么变化，判断挑战卡在哪一步。
	for i := 0; i < 15; i++ {
		time.Sleep(2 * time.Second)
		var h, t string
		var l int
		_ = chromedp.Run(runCtx, chromedp.Evaluate(`location.href`, &h))
		_ = chromedp.Run(runCtx, chromedp.Evaluate(`document.title`, &t))
		_ = chromedp.Run(runCtx, chromedp.Evaluate(`document.documentElement ? document.documentElement.outerHTML.length : -1`, &l))
		fmt.Printf("   [t=%2ds] len=%6d title=%q href=%s\n", (i+1)*2, l, t, h)
		if l > 2000 && t != "" {
			fmt.Println("   >> 页面看起来已加载出真实内容，提前结束轮询")
			break
		}
	}

	// 诊断：页面里 fetch 同地址看状态码（200=挑战已过 / 412=仍被 WAF 拦），
	// 以及 document.cookie 里 WAF 下发的 cookie 是否被挑战脚本更新。
	var fetchInfo, cookieStr string
	_ = chromedp.Run(runCtx, chromedp.Evaluate(`fetch(location.href).then(function(r){
		return r.text().then(function(b){ return 'status='+r.status+' body_len='+b.length+' head='+b.slice(0,120).replace(/\n/g,' '); });
	})`, &fetchInfo))
	_ = chromedp.Run(runCtx, chromedp.Evaluate(`document.cookie`, &cookieStr))
	fmt.Println(">> [诊断] fetch 同地址:", fetchInfo)
	fmt.Println(">> [诊断] document.cookie:", cookieStr)

	// 指纹诊断：看瑞数挑战脚本视角下的浏览器环境长什么样
	var fp string
	_ = chromedp.Run(runCtx, chromedp.Evaluate(`JSON.stringify((function(){
		var gl = null;
		try {
			var c = document.createElement('canvas');
			var g = c.getContext('webgl') || c.getContext('experimental-webgl');
			if (g) {
				var dbg = g.getExtension('WEBGL_debug_renderer_info');
				gl = dbg ? g.getParameter(dbg.UNMASKED_RENDERER_WEBGL) : g.getParameter(g.RENDERER);
			}
		} catch(e){ gl = 'err:'+e.message; }
		return {
			webdriver: navigator.webdriver,
			ua: navigator.userAgent,
			plugins: navigator.plugins.length,
			languages: navigator.languages,
			platform: navigator.platform,
			hasChrome: !!window.chrome,
			screenW: screen.width, screenH: screen.height,
			outerW: window.outerWidth, outerH: window.outerHeight,
			webgl: gl
		};
	})(), null, 2)`, &fp))
	fmt.Println(">> [诊断] 浏览器指纹:", fp)

	var href, title, html string
	_ = chromedp.Run(runCtx, chromedp.Evaluate(`location.href`, &href))
	_ = chromedp.Run(runCtx, chromedp.Evaluate(`document.title`, &title))
	if err := chromedp.Run(runCtx, chromedp.Evaluate(`document.documentElement.outerHTML`, &html)); err != nil {
		fmt.Printf("!! 取 HTML 失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf(">> href=%s title=%q html_len=%d\n", href, title, len(html))

	// 整页截图（无头/可见都支持）
	var fullshot []byte
	if err := chromedp.Run(runCtx, chromedp.FullScreenshot(&fullshot, 90)); err == nil && len(fullshot) > 0 {
		os.WriteFile(*outDir+"/login.png", fullshot, 0644)
		fmt.Printf(">> 整页截图 login.png (%d 字节)\n", len(fullshot))
	}

	// 枚举所有 input / button / img，打印摘要，便于确认表单结构
	var summary string
	_ = chromedp.Run(runCtx, chromedp.Evaluate(`JSON.stringify((function(){
		var inputs = Array.from(document.querySelectorAll('input')).map(function(el){
			return {tag:'input', type:el.type||'', name:el.name||'', id:el.id||'', placeholder:el.placeholder||'', visible: el.offsetParent!==null};
		});
		var buttons = Array.from(document.querySelectorAll('button, input[type=submit], input[type=button], a')).map(function(el){
			return {tag:el.tagName, text:(el.innerText||el.value||'').trim().slice(0,20), id:el.id||'', visible: el.offsetParent!==null};
		});
		var imgs = Array.from(document.querySelectorAll('img')).map(function(el){
			return {tag:'img', src:(el.src||'').slice(0,120), id:el.id||'', cls:el.className||'', w:el.width, h:el.height};
		});
		var iframes = Array.from(document.querySelectorAll('iframe')).map(function(el){
			return {tag:'iframe', src:(el.src||'').slice(0,120), id:el.id||''};
		});
		return {inputs:inputs, buttons:buttons, imgs:imgs, iframes:iframes};
	})())`, &summary))
	fmt.Println(">> 表单元素摘要:")
	fmt.Println(summary)

	// 尝试找验证码图片：常见 src 关键字 code/captcha/verify/rand/img
	var captchaPath string
	_ = chromedp.Run(runCtx, chromedp.Evaluate(`(function(){
		var imgs = Array.from(document.querySelectorAll('img'));
		var el = imgs.find(function(im){
			var s = (im.src||'').toLowerCase();
			return s.indexOf('code')>=0 || s.indexOf('captcha')>=0 || s.indexOf('verify')>=0 || s.indexOf('rand')>=0 || s.indexOf('validate')>=0;
		});
		if (!el) return '';
		el.scrollIntoView({block:'center'});
		return el.src || '';
	})()`, &captchaPath))
	if captchaPath != "" {
		fmt.Printf(">> 疑似验证码图片: %s\n", captchaPath)
		// 验证码元素截图（裁出验证码本体，供 OCR 用）：
		// toDataURL 返回 "data:image/png;base64,xxx"，需去掉前缀并 base64 解码
		var dataURL string
		if err := chromedp.Run(runCtx, chromedp.Evaluate(`(function(){
			var imgs = Array.from(document.querySelectorAll('img'));
			var el = imgs.find(function(im){
				var s = (im.src||'').toLowerCase();
				return s.indexOf('code')>=0 || s.indexOf('captcha')>=0 || s.indexOf('verify')>=0 || s.indexOf('rand')>=0 || s.indexOf('validate')>=0;
			});
			if (!el) return '';
			var r = el.getBoundingClientRect();
			var c = document.createElement('canvas');
			c.width = Math.max(1, Math.round(r.width));
			c.height = Math.max(1, Math.round(r.height));
			var g = c.getContext('2d');
			g.drawImage(el, 0, 0, c.width, c.height);
			return c.toDataURL('image/png');
		})()`, &dataURL)); err == nil && dataURL != "" {
			if idx := indexOf(dataURL, ","); idx >= 0 {
				if png, err := base64.StdEncoding.DecodeString(dataURL[idx+1:]); err == nil {
					os.WriteFile(*outDir+"/captcha.png", png, 0644)
					fmt.Printf(">> 验证码已存 captcha.png (%d 字节)\n", len(png))
				} else {
					fmt.Printf("!! base64 解码失败: %v\n", err)
				}
			}
		}
	} else {
		fmt.Println(">> 未找到疑似验证码图片（src 关键字不匹配），看 imgs 摘要人工判断")
	}

	os.WriteFile(*outDir+"/page.html", []byte(html), 0644)
	fmt.Printf(">> HTML 已存 page.html (%d 字节)\n", len(html))
}
