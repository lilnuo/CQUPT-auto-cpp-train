package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"main/ai"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// stdin 统一从标准输入读取，供交互式输入与登录时的"按回车继续"共用，避免多个 reader 冲突
var stdin = bufio.NewReader(os.Stdin)

func main() {
	dump := flag.Bool("dump", false, "进入做题页后把页面 HTML 保存到 page.html，用于调试选择器")
	flag.Parse()

	if *dump {
		// dump 只抓取页面结构、不需要调用大模型，因此跳过 AI 初始化
		// （这样即使 .env 没填 ARK_API_KEY 也能跑 dump）
		log.Println("[dump 模式] 跳过 AI 初始化，无需填写 ARK_API_KEY")
	} else {
		if err := ai.InitAI(); err != nil {
			log.Fatalf("ai初始化失败: %v（请检查 .env 中的 ARK_API_KEY 与 ARK_MODEL_ID）", err)
		}
	}

	fmt.Println("请输入你的学号")
	username := readLine()
	fmt.Println("请输入你的密码")
	password := readLine()
	fmt.Println("输入你想刷的题目数量")
	numStr := readLine()
	num, err := strconv.Atoi(strings.TrimSpace(numStr))
	if err != nil || num <= 0 {
		log.Fatalf("题目数量必须是一个正整数，你输入的是 %q", numStr)
	}

	// 新增模式分流（见 progap.go）：默认 quiz 走原有整页刷题逻辑，行为不变；
	// -mode=progap / progapdump 才进入程序片段编程题流程。
	var total int
	if *modeFlag == "progap" || *modeFlag == "progapdump" {
		total, err = runProgap(username, password, num, *modeFlag == "progapdump")
	} else {
		total, err = runTrainer(username, password, num, *dump)
	}
	if err != nil {
		log.Fatalf("刷题过程出错: %v", err)
	}
	fmt.Printf("最终得分为 %d 分\n", total)
}

// readLine 读取一行并去除首尾空白
func readLine() string {
	line, _ := stdin.ReadString('\n')
	return strings.TrimSpace(line)
}

// runTrainer 启动一个浏览器实例，登录一次后串行刷题。
// 每次进入做题页后自动检测题型：编程题（Monaco）或单选填空题（整页多题滚动）。
//
// 注意：新站点有瑞数 WAF——它会检测"CDP 附加时的自动化副作用"并拒绝渲染页面
// （这就是之前一切"白屏/39 字节空壳"的根因）。验证过的可行流程是：
// 原生启动 Chrome（不附加任何自动化连接）→ 等 WAF 挑战自动通过 → 再附加 chromedp。
func runTrainer(username, password string, num int, dump bool) (int, error) {
	if cdp := os.Getenv("CDP_URL"); cdp != "" {
		// 高级模式：接入你自己已打开并登录的浏览器（需开启远程调试端口）
		log.Printf("已接入远程浏览器 %s，假设已登录，跳过自动登录", cdp)
		alloctx, cancelAlloc := chromedp.NewRemoteAllocator(context.Background(), cdp)
		defer cancelAlloc()
		ctx, cancelCtx := chromedp.NewContext(alloctx)
		defer cancelCtx()
		return solveLoop(ctx, num, dump)
	}

	// 1) 原生启动 Chrome：启动参数打开登录页的那个标签页全程无任何 CDP 附加，
	//    对 WAF 而言与真人打开无异，挑战可以自动通过。
	//    同时从 stderr 读取浏览器级 WebSocket 地址（"DevTools listening on ws://..."）。
	port := os.Getenv("CDP_PORT")
	if port == "" {
		port = "9223"
	}
	chromeCmd, browserWS, err := launchHumanChrome(port, userDataDir(), loginURL)
	if err != nil {
		return 0, err
	}
	defer func() {
		_ = chromeCmd.Process.Kill()
		_ = chromeCmd.Wait()
	}()

	// allocator 地址：优先用 stderr 读到的 ws://，兜底 http://127.0.0.1:port
	allocURL := "http://127.0.0.1:" + port
	if browserWS != "" {
		allocURL = browserWS
	}
	alloctx, cancelAlloc := chromedp.NewRemoteAllocator(context.Background(), allocURL)
	defer cancelAlloc()

	// 2) 立刻在一个【新标签页】建立 CDP 连接：连接在启动窗口期建立后会长期保持，
	//    后续所有操作都走这条已建立的连接，不再新建任何本机连接。
	//    原生标签页没有任何 CDP 会话，WAF 挑战在那里不受干扰地完成。
	ctx, cancelCtx := chromedp.NewContext(alloctx)
	defer cancelCtx()
	if err := safeRun(ctx); err != nil {
		return 0, fmt.Errorf("建立浏览器控制连接失败: %w", err)
	}

	// 3) 固定等待：原生标签页无 CDP 干扰，挑战一般 10~15 秒内自动完成
	log.Printf("等待 WAF 挑战自动通过（原生标签页无干扰加载中，约 20 秒）...")
	time.Sleep(20 * time.Second)

	// 4) 我们的标签页导航到登录页：此时浏览器内已有合法 WAF cookie，
	//    服务器直接返回真实页面、不再下发挑战，也就不会触发 CDP 检测。
	if err := ensureLoginPage(ctx); err != nil {
		return 0, err
	}

	// 5) 自动登录：填学号密码 → 截图验证码 → ddddocr 识别 → 提交 → 校验，失败自动重试
	if err := autoLogin(ctx, username, password); err != nil {
		return 0, fmt.Errorf("登录失败: %w", err)
	}

	// 6) 登录后停在 main.jsp 作业卡片列表：选一张卡进入答题页
	//    （默认选标题含"刷题"的卡，可用 ASSIGN_KEYWORD 环境变量改）
	//    dump 模式下若进不去，也照样把当前页 dump 出来方便排查
	quizCtx, cancelQuiz, err := enterAssignment(ctx, alloctx)
	if err != nil {
		if dump {
			log.Printf("进入作业失败（%v），直接 dump 当前页面", err)
			dumpPage(ctx)
			return 0, nil
		}
		return 0, fmt.Errorf("进入作业失败: %w", err)
	}
	if cancelQuiz != nil {
		defer cancelQuiz()
	}

	return solveLoop(quizCtx, num, dump)
}

// ensureLoginPage 导航到登录页并校验拿到的是真实页面（挑战未过时页面为空壳，等待重试）
func ensureLoginPage(ctx context.Context) error {
	for i := 1; i <= 3; i++ {
		navCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		err := safeRun(navCtx, chromedp.Navigate(loginURL), chromedp.Sleep(5*time.Second))
		var title string
		var l int
		_ = safeRun(navCtx, chromedp.Evaluate(`document.title`, &title))
		_ = safeRun(navCtx, chromedp.Evaluate(`document.documentElement ? document.documentElement.outerHTML.length : -1`, &l))
		cancel()
		if err == nil && l > 2000 {
			log.Printf("登录页就绪: title=%q len=%d", title, l)
			return nil
		}
		log.Printf("登录页内容异常（len=%d title=%q err=%v），挑战可能仍在进行，10s 后重试（%d/3）", l, title, err, i)
		time.Sleep(10 * time.Second)
	}
	return fmt.Errorf("多次尝试后登录页仍不可用：WAF 挑战可能未通过，请重试")
}

// solveLoop 登录后的主流程：dump 模式直接把当前页面存成 page.html；
// 否则自动检测题型并作答（编程题逐题 / 整页填空题一次做完）。
func solveLoop(ctx context.Context, num int, dump bool) (int, error) {
	if dump {
		dumpPage(ctx)
		return 0, nil
	}

	total := 0
	for i := 0; i < num; i++ {
		switch detectMode(ctx) {
		case "code":
			s, finished, err := solveCode(ctx)
			if err != nil {
				log.Printf("第 %d 题处理出错: %v", i+1, err)
				continue
			}
			if finished {
				log.Printf("题目已全部完成，提前结束")
				continue
			}
			total += s
		case "quiz":
			// 整页选择/填空题：最多做 num 道未提交的，做完读总分
			s, err := solveQuizPage(ctx, num)
			if err != nil {
				return total, fmt.Errorf("填空题页面处理失败: %w", err)
			}
			return s, nil
		default:
			return total, fmt.Errorf("无法识别题目类型，可运行 go run . -dump 保存页面结构后检查")
		}
	}
	return total, nil
}

const loginURL = "https://prg.cqupt.edu.cn/indexcs/simple.jsp?loginErr=0"

// ===================== 进入作业（登录后主页是作业卡片列表） =====================

// assignmentCardsJS 枚举 main.jsp 上的作业卡片：找文本含"进入作业/开始答题"的可见按钮，
// 向上爬取所属卡片的标题行（排除"作业"标签、作业时间、截止等行）。
// 返回 [{i, title, text, href}]。
const assignmentCardsJS = `(function(){
  var out = [];
  Array.from(document.querySelectorAll('a,button')).forEach(function(b){
    var t = (b.innerText || '').trim();
    if (!/进入作业|开始答题|进入答题/.test(t)) return;
    if (b.offsetParent === null) return;
    var card = b;
    for (var k = 0; k < 8 && card.parentElement; k++) {
      card = card.parentElement;
      if ((card.innerText || '').length > 60) break;
    }
    var lines = (card.innerText || '').split('\n').map(function(s){ return s.trim(); })
      .filter(function(s){ return s !== '' && s !== '作业' && !/进入作业|开始答题|作业时间|截止/.test(s); });
    out.push({i: out.length, title: (lines[0] || '').slice(0, 60), text: t, href: b.href || ''});
  });
  return JSON.stringify(out);
})()`

// assignmentClickJS 点击第 idx 张作业卡的"进入作业"按钮（与上面枚举同序）
func assignmentClickJS(idx int) string {
	return fmt.Sprintf(`(function(){
  var btns = [];
  Array.from(document.querySelectorAll('a,button')).forEach(function(b){
    var t = (b.innerText || '').trim();
    if (/进入作业|开始答题|进入答题/.test(t) && b.offsetParent !== null) btns.push(b);
  });
  var b = btns[%d];
  if (!b) return 'no-btn';
  b.scrollIntoView({block: 'center'});
  b.click();
  return 'ok';
})()`, idx)
}

// enterAssignment 登录后在 main.jsp 选择作业卡并进入答题页。
// 默认进入标题含 ASSIGN_KEYWORD（默认"刷题"）的卡；没有匹配则进第一张并提示。
// 返回答题页上下文：若点击后开的是新标签页，会附加到新 target 并返回其 ctx（调用方负责 cancel）。
func enterAssignment(ctx, alloctx context.Context) (context.Context, context.CancelFunc, error) {
	// 等主页渲染出作业卡（最多 40s）
	var raw string
	var cards []struct {
		I     int    `json:"i"`
		Title string `json:"title"`
		Text  string `json:"text"`
		Href  string `json:"href"`
	}
	found := false
	for i := 0; i < 20; i++ {
		pollCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		_ = safeRun(pollCtx, chromedp.Evaluate(assignmentCardsJS, &raw))
		cancel()
		if err := json.Unmarshal([]byte(raw), &cards); err == nil && len(cards) > 0 {
			found = true
			break
		}
		time.Sleep(2 * time.Second)
	}
	if !found {
		return nil, nil, fmt.Errorf("主页上没有找到任何'进入作业'按钮（可能页面未渲染，可 go run . -dump 查看）")
	}

	for _, c := range cards {
		log.Printf("发现作业卡: [%d] %s（按钮 %q）", c.I, c.Title, c.Text)
	}

	keyword := os.Getenv("ASSIGN_KEYWORD")
	if keyword == "" {
		keyword = "刷题"
	}
	choice := -1
	for _, c := range cards {
		if strings.Contains(c.Title, keyword) {
			choice = c.I
			break
		}
	}
	if choice < 0 {
		choice = cards[0].I
		log.Printf("没有标题含 %q 的作业卡，默认进入第一张（可用 ASSIGN_KEYWORD 环境变量指定）", keyword)
	}
	log.Printf("进入作业卡 [%d] %s", choice, cards[choice].Title)

	// 记录点击前的 URL 和已有标签页，用于判断是同页跳转还是新开标签页
	var beforeHref string
	_ = safeRun(ctx, chromedp.Evaluate(`location.href`, &beforeHref))
	beforeTargets := map[target.ID]bool{}
	if ts, err := chromedp.Targets(ctx); err == nil {
		for _, t := range ts {
			beforeTargets[t.TargetID] = true
		}
	}

	clickCtx, cancelClick := context.WithTimeout(ctx, 10*time.Second)
	var resp string
	_ = safeRun(clickCtx, chromedp.Evaluate(assignmentClickJS(choice), &resp))
	cancelClick()
	if resp != "ok" {
		return nil, nil, fmt.Errorf("点击'进入作业'失败: %s", resp)
	}

	// 等结果：同页跳转（URL 变化）或新开标签页（出现新 target），最多 15s
	for i := 0; i < 15; i++ {
		time.Sleep(1 * time.Second)

		// 情况 1：同页跳转
		var nowHref string
		pollCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_ = safeRun(pollCtx, chromedp.Evaluate(`location.href`, &nowHref))
		cancel()
		if nowHref != "" && nowHref != beforeHref && !strings.Contains(nowHref, "main.jsp") {
			log.Printf("已进入答题页（同页跳转）: %s", nowHref)
			return ctx, nil, nil
		}

		// 情况 2：新开了标签页
		if ts, err := chromedp.Targets(ctx); err == nil {
			for _, t := range ts {
				if t.Type != "page" || beforeTargets[t.TargetID] {
					continue
				}
				newCtx, newCancel := chromedp.NewContext(alloctx, chromedp.WithTargetID(t.TargetID))
				if err := safeRun(newCtx); err != nil {
					newCancel()
					continue
				}
				log.Printf("已进入答题页（新标签页）: %s", t.URL)
				return newCtx, newCancel, nil
			}
		}
	}
	return nil, nil, fmt.Errorf("点击'进入作业'后页面没有变化（超时 15s）")
}

// ===================== 浏览器启动 / WAF 探测 / 附加 =====================

// findChrome 查找 Chrome 可执行文件（支持 CHROME_PATH 覆盖）
func findChrome() (string, error) {
	if p := os.Getenv("CHROME_PATH"); p != "" {
		return p, nil
	}
	candidates := []string{
		"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
		"/Applications/Chromium.app/Contents/MacOS/Chromium",
		"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
		"C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe",
		"C:\\Program Files (x86)\\Google\\Chrome\\Application\\chrome.exe",
		"/usr/bin/google-chrome",
		"/usr/bin/chromium",
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	if p, err := exec.LookPath("google-chrome"); err == nil {
		return p, nil
	}
	if p, err := exec.LookPath("chromium"); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("未找到 Chrome/Chromium/Edge 浏览器，请安装或用 CHROME_PATH 指定路径")
}

// launchHumanChrome 以"真人方式"原生启动 Chrome：只开调试端口，不建立任何 CDP 会话。
// 瑞数 WAF 检测的是 CDP 附加时的自动化副作用，这样启动时页面加载与真人无异。
// 返回：进程句柄 + 浏览器级 WebSocket 地址（从 stderr 的 "DevTools listening on ws://..." 读到，可能为空）。
func launchHumanChrome(port, profile, url string) (*exec.Cmd, string, error) {
	bin, err := findChrome()
	if err != nil {
		return nil, "", err
	}
	log.Printf("使用浏览器: %s", bin)
	cmd := exec.Command(bin,
		"--remote-debugging-port="+port,
		"--user-data-dir="+profile,
		"--no-first-run",
		"--no-default-browser-check",
		url,
	)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, "", fmt.Errorf("获取 Chrome stderr 失败: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, "", fmt.Errorf("启动 Chrome 失败: %w", err)
	}

	// 持续读 stderr：找到 DevTools WebSocket 地址后回报；之后继续读并丢弃，
	// 防止管道缓冲区写满导致 Chrome 阻塞。
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
	case <-time.After(15 * time.Second):
		// 没读到也不致命：调用方会退回 http://127.0.0.1:port
		return cmd, "", nil
	}
}

// safeRun 包装 chromedp.Run：chromedp v0.14.2 的 RemoteAllocator 在远程连接失败时
// 会 panic（close of closed channel）而不是返回错误，这里 recover 转成正常错误。
func safeRun(ctx context.Context, actions ...chromedp.Action) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("浏览器操作异常（可能是远程连接被中断）: %v", r)
		}
	}()
	return chromedp.Run(ctx, actions...)
}

// ===================== 自动登录（填表 + OCR 验证码 + 重试） =====================

const maxLoginTry = 4

// autoLogin 自动完成登录：填学号密码 → 截图验证码 → ddddocr 识别 → 提交 → 校验结果，
// 失败自动刷新验证码重试；OCR 多次不过或 OCR 不可用时转人工兜底。
func autoLogin(ctx context.Context, username, password string) error {
	// 先判断是否已处于登录态：登录页被重定向（表单迟迟不出现 + URL 离开登录页）
	// 说明 Cookie 有效，直接跳过登录。
	for i := 0; i < 8; i++ {
		var hasForm bool
		var href string
		checkCtx, cancelCheck := context.WithTimeout(ctx, 5*time.Second)
		_ = safeRun(checkCtx,
			chromedp.Evaluate(`!!document.querySelector('#username')`, &hasForm),
			chromedp.Evaluate(`location.href`, &href),
		)
		cancelCheck()
		if hasForm {
			break // 表单在，走正常登录流程
		}
		if !strings.Contains(href, "simple.jsp") {
			fmt.Printf(">>> 检测到已有登录态（Cookie 有效，当前在 %s），跳过登录\n", href)
			return nil
		}
		time.Sleep(2 * time.Second)
	}

	// 等登录表单就绪（附加后标签页可能还在自刷新）
	readyCtx, cancelReady := context.WithTimeout(ctx, 20*time.Second)
	err := safeRun(readyCtx, chromedp.WaitReady(`#username`, chromedp.ByQuery))
	cancelReady()
	if err != nil {
		return fmt.Errorf("等待登录表单就绪失败: %w", err)
	}

	for i := 1; i <= maxLoginTry; i++ {
		if err := fillInput(ctx, "#username", username); err != nil {
			return err
		}
		if err := fillInput(ctx, "#password", password); err != nil {
			return err
		}

		// 截图验证码（所见即所得）
		var png []byte
		capCtx, cancelCap := context.WithTimeout(ctx, 15*time.Second)
		err := chromedp.Run(capCtx, chromedp.Screenshot(`img[src="/cgjiaoyan"]`, &png, chromedp.NodeVisible, chromedp.ByQuery))
		cancelCap()
		if err != nil {
			log.Printf("截图验证码失败（%v），转人工登录", err)
			return manualLoginFallback(ctx)
		}
		imgPath := filepath.Join(os.TempDir(), "cqupt_captcha.png")
		if err := os.WriteFile(imgPath, png, 0644); err != nil {
			return fmt.Errorf("保存验证码图片失败: %w", err)
		}

		code, err := ocrCaptcha(imgPath)
		if err != nil {
			log.Printf("验证码 OCR 失败（%v），转人工登录", err)
			return manualLoginFallback(ctx)
		}
		log.Printf("第 %d 次登录尝试：OCR 验证码 = %q", i, code)
		if err := fillInput(ctx, "#captchaCode", code); err != nil {
			return err
		}

		submitCtx, cancelSub := context.WithTimeout(ctx, 10*time.Second)
		_ = chromedp.Run(submitCtx, chromedp.Click(`#cgstuloginbtn`, chromedp.ByQuery))
		cancelSub()

		ok, reason := waitLoginResult(ctx, 8*time.Second)
		if ok {
			fmt.Println(">>> 自动登录成功！")
			return nil
		}
		log.Printf("登录未成功（%s），刷新验证码后重试...", reason)
		// 点击验证码图片换新码
		refreshCtx, cancelRef := context.WithTimeout(ctx, 10*time.Second)
		_ = chromedp.Run(refreshCtx, chromedp.Click(`img[src="/cgjiaoyan"]`, chromedp.ByQuery))
		cancelRef()
		time.Sleep(1500 * time.Millisecond)
	}
	// OCR 多次不过（可能验证码区分大小写），转人工兜底：浏览器是可见的，用户可直接输
	log.Printf("OCR 连续 %d 次未过，转人工兜底", maxLoginTry)
	return manualLoginFallback(ctx)
}

// fillInput 用原生 setter + 事件给输入框填值（兼容各种前端框架的表单监听）
func fillInput(ctx context.Context, sel, val string) error {
	valBytes, _ := json.Marshal(val)
	js := fmt.Sprintf(`(function(){
		var el = document.querySelector(%q);
		if (!el) return 'no-el';
		el.focus();
		var set = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value').set;
		set.call(el, %s);
		el.dispatchEvent(new Event('input', {bubbles: true}));
		el.dispatchEvent(new Event('change', {bubbles: true}));
		el.blur();
		return 'ok';
	})()`, sel, string(valBytes))
	fillCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var resp string
	if err := chromedp.Run(fillCtx, chromedp.Evaluate(js, &resp)); err != nil {
		return fmt.Errorf("填写 %s 失败: %w", sel, err)
	}
	if resp != "ok" {
		return fmt.Errorf("页面上未找到输入框 %s", sel)
	}
	return nil
}

// waitLoginResult 提交后轮询登录结果：密码框消失=成功；URL 带 loginErr=1=失败
func waitLoginResult(ctx context.Context, d time.Duration) (bool, string) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		pollCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		var href string
		var hasPwd bool
		_ = chromedp.Run(pollCtx, chromedp.Evaluate(`location.href`, &href))
		_ = chromedp.Run(pollCtx, chromedp.Evaluate(`!!document.querySelector('#password')`, &hasPwd))
		cancel()
		if strings.Contains(href, "loginErr=1") {
			return false, "loginErr=1（验证码错误，或学号/密码错误）"
		}
		if !hasPwd && !strings.Contains(href, "loginErr") {
			return true, ""
		}
		time.Sleep(800 * time.Millisecond)
	}
	return false, "等待登录结果超时"
}

// ocrCaptcha 调用 ddddocr 识别验证码图片。
// python 优先级：PYTHON_BIN 环境变量 > 本机隔离 venv > 系统 python3。
func ocrCaptcha(imgPath string) (string, error) {
	py := os.Getenv("PYTHON_BIN")
	if py == "" {
		venv := "/Users/lilinuo/.workbuddy/binaries/python/envs/default/bin/python"
		if _, err := os.Stat(venv); err == nil {
			py = venv
		} else {
			py = "python3"
		}
	}
	out, err := exec.Command(py, "ocr/ocr_captcha.py", imgPath).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("OCR 执行失败: %v（输出: %s）", err, strings.TrimSpace(string(out)))
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	code := strings.TrimSpace(lines[len(lines)-1])
	if len(code) < 3 || len(code) > 8 {
		return "", fmt.Errorf("OCR 结果可疑 %q", code)
	}
	return code, nil
}

// manualLoginFallback 人工兜底：OCR 不可用时，用户在可见浏览器里手动登录，
// 脚本检测密码框消失即继续；也可以在终端按回车强制继续。
func manualLoginFallback(ctx context.Context) error {
	fmt.Println(">>> 请在浏览器里手动输入学号、密码、验证码并登录。")
	fmt.Println(">>> 登录完成后脚本会自动检测继续；也可以在此按【回车】继续。")
	manualCh := make(chan struct{})
	go func() {
		stdin.ReadString('\n')
		close(manualCh)
	}()
	deadline := time.Now().Add(10 * time.Minute)
	seenForm := false
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		pollCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		var hasPwd bool
		_ = chromedp.Run(pollCtx, chromedp.Evaluate(`!!document.querySelector('#password')`, &hasPwd))
		cancel()
		if hasPwd {
			seenForm = true
		}
		if seenForm && !hasPwd {
			fmt.Println(">>> 检测到已登录，开始自动答题")
			return nil
		}
		select {
		case <-manualCh:
			fmt.Println(">>> 收到手动继续信号，开始自动答题")
			return nil
		case <-ticker.C:
			if time.Now().After(deadline) {
				return fmt.Errorf("等待登录超时（10 分钟）")
			}
		}
	}
}

// detectMode 检测当前做题页类型：code（Monaco 编程题）或 quiz（整页单选填空题）
func detectMode(ctx context.Context) string {
	modeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// 等页面渲染稳定后再判断
	_ = chromedp.Run(modeCtx, chromedp.Sleep(2*time.Second))

	var isCode, isQuiz bool
	_ = chromedp.Run(modeCtx,
		chromedp.Evaluate(`!!(window.monaco && monaco.editor && monaco.editor.getEditors && monaco.editor.getEditors().length > 0)`, &isCode),
		chromedp.Evaluate(`(function(){
			var t = document.body.innerText || '';
			if (t.indexOf('单选填空') >= 0 || t.indexOf('单项选择题') >= 0) return true;
			return document.querySelectorAll('input[type="text"], input:not([type])').length > 0;
		})()`, &isQuiz),
	)
	switch {
	case isCode:
		return "code"
	case isQuiz:
		return "quiz"
	default:
		return "unknown"
	}
}

// solveCode 处理编程题（Monaco 编辑器）：取题干 -> AI 生成代码 -> 填入 -> 提交 -> 读分数
func solveCode(ctx context.Context) (score int, finished bool, err error) {
	mainCtx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()

	// 未刷完，获取题干
	var question string
	if err = chromedp.Run(mainCtx,
		chromedp.Text(`.q-content`, &question, chromedp.ByQuery),
	); err != nil {
		return 0, false, fmt.Errorf("获取题目失败: %w", err)
	}

	answer, err := ai.Answer(question)
	if err != nil {
		return 0, false, fmt.Errorf("答案生成失败: %w", err)
	}

	// 用 json.Marshal 把代码转成合法 JS 字符串字面量后注入 Monaco 编辑器，
	// 规避代码内引号/换行的转义问题
	codeBytes, _ := json.Marshal(answer)
	fillCodeJS := fmt.Sprintf(`monaco.editor.getEditors()[0].setValue(%s)`, string(codeBytes))

	var scoreStr string
	if err = chromedp.Run(mainCtx,
		chromedp.WaitVisible(`.monaco-editor`, chromedp.ByQuery),
		chromedp.Evaluate(fillCodeJS, nil),
		chromedp.WaitVisible(`//button[contains(., "提交")]`, chromedp.BySearch),
		chromedp.Click(`//button[contains(., "提交")]`, chromedp.BySearch),
		chromedp.WaitVisible(`//button[contains(., "确定")]`, chromedp.BySearch),
		chromedp.Click(`//button[contains(., "确定")]`, chromedp.BySearch),
		chromedp.WaitVisible(`tr[role="row"] td.mat-column-score`),
		chromedp.Text(`tr[role="row"] td.mat-column-score`, &scoreStr),
	); err != nil {
		return 0, false, fmt.Errorf("填充代码或提交失败: %w", err)
	}

	scoreInt, _ := strconv.Atoi(strings.TrimSpace(scoreStr))
	return scoreInt, false, nil
}

// quizListJS 枚举页面上所有内嵌题目表单（真实 DOM：form[name=answerForm{题号}]）。
// 两种题干结构统一处理：选择题在 div[id^=tts{pid}text] 里、cloze 填空题直接是 form 内 <p>——
// 都包含在 form 内，所以统一克隆 form、去掉输入控件后取 innerText。
// 返回 [{pid, text, n, answered}]。
const quizListJS = `(function(){
  var out = [];
  document.querySelectorAll('form[name^="answerForm"]').forEach(function(f){
    var m = f.name.match(/answerForm(\d+)/);
    if (!m) return;
    var pid = m[1];
    var c = f.cloneNode(true);
    c.querySelectorAll('input,button,select,textarea,script').forEach(function(e){ e.remove(); });
    var text = (c.innerText || '').trim().slice(0, 1500);
    var inputs = f.querySelectorAll('input[name^="answer"]');
    var answered = false;
    inputs.forEach(function(inp){ if ((inp.value||'').trim() !== '') answered = true; });
    var tip = document.getElementById('saveTip'+pid);
    if (tip && (tip.innerText||'').indexOf('已提交') >= 0) answered = true;
    if (inputs.length === 0) return;
    out.push({pid: pid, text: text, n: inputs.length, answered: answered});
  });
  return JSON.stringify(out);
})()`

// quizFillJS 填入第 pid 题的答案并触发自动提交：
// 全部输入框用原生 setter 填好值后，在最后一个 input 上派发 input + change 事件
// （选择题 oninput=form.submit()；cloze 填空 oninput=cgClozeInput + onchange=cgClozeChange，
//
//	提交到隐藏 iframe frame_problemhandler，页面不刷新）。
func quizFillJS(pid string, answers []string) string {
	ansBytes, _ := json.Marshal(answers)
	return fmt.Sprintf(`(function(){
  var f = document.forms['answerForm%[1]s'];
  if (!f) return 'no-form';
  var inputs = Array.from(f.querySelectorAll('input[name^="answer"]'));
  if (inputs.length !== %[2]d) return 'count-mismatch:'+inputs.length;
  var answers = %[3]s;
  var set = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value').set;
  for (var i = 0; i < inputs.length; i++){
    inputs[i].scrollIntoView({block: 'center'});
    set.call(inputs[i], answers[i] || '');
  }
  var last = inputs[inputs.length-1];
  last.dispatchEvent(new Event('input', {bubbles: true}));
  last.dispatchEvent(new Event('change', {bubbles: true}));
  return 'ok';
})()`, pid, len(answers), string(ansBytes))
}

// quizCheckJS 检查第 pid 题的 saveTip 是否已变为"已提交"
func quizCheckJS(pid string) string {
	return fmt.Sprintf(`(function(){
  var tip = document.getElementById('saveTip%s');
  if (!tip) return 'no-tip';
  return (tip.innerText || '').indexOf('已提交') >= 0 ? 'ok' : 'pending';
})()`, pid)
}

// quizScoreJS 从页面读取 "总分: x.xx"
const quizScoreJS = `(function(){
  var m = (document.body.innerText || '').match(/总分[:：]\s*([\d.]+)/);
  return m ? m[1] : '';
})()`

// solveQuizPage 处理主页上的整卷选择/填空题（最多做 num 道未提交的）：
// 等页面渲染 -> 枚举题目 -> 逐题 AI 作答并填入（自动提交）-> 校验已提交 -> 读总分
func solveQuizPage(ctx context.Context, num int) (int, error) {
	quizCtx, cancel := context.WithTimeout(ctx, 60*time.Minute)
	defer cancel()

	// 等动态渲染：轮询直到题目表单出现（最多 60s）
	var raw string
	var items []struct {
		Pid      string `json:"pid"`
		Text     string `json:"text"`
		N        int    `json:"n"`
		Answered bool   `json:"answered"`
	}
	for i := 0; i < 30; i++ {
		if err := safeRun(quizCtx, chromedp.Evaluate(quizListJS, &raw)); err != nil {
			return 0, fmt.Errorf("读取题目列表失败: %w", err)
		}
		if err := json.Unmarshal([]byte(raw), &items); err != nil {
			return 0, fmt.Errorf("解析题目列表失败: %w", err)
		}
		if len(items) > 0 {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if len(items) == 0 {
		return 0, fmt.Errorf("页面上没有找到题目表单（answerForm），可能不在作业页上")
	}

	unanswered := 0
	for _, it := range items {
		if !it.Answered {
			unanswered++
		}
	}
	log.Printf("共 %d 道题，其中 %d 道未提交，本次最多做 %d 道", len(items), unanswered, num)

	done, failed := 0, 0
	for _, it := range items {
		if done >= num {
			break
		}
		if it.Answered || strings.TrimSpace(it.Text) == "" {
			continue
		}
		done++
		fmt.Printf("[%d] 题 %s 解答中（%d 个空）...\n", done, it.Pid, it.N)

		var answers []string
		var err error
		if it.N == 1 {
			var letter string
			letter, err = ai.AnswerChoice(it.Text)
			answers = []string{letter}
		} else {
			answers, err = ai.AnswerMulti(it.Text, it.N)
		}
		if err != nil {
			log.Printf("题 %s 生成答案失败: %v", it.Pid, err)
			failed++
			continue
		}

		var resp string
		if err := safeRun(quizCtx, chromedp.Evaluate(quizFillJS(it.Pid, answers), &resp)); err != nil || resp != "ok" {
			log.Printf("题 %s 填入失败: %v %s", it.Pid, err, resp)
			failed++
			continue
		}

		// 等自动提交生效（saveTip 变"已提交"），最多 8 秒
		ok := false
		for i := 0; i < 8; i++ {
			time.Sleep(1 * time.Second)
			var st string
			_ = safeRun(quizCtx, chromedp.Evaluate(quizCheckJS(it.Pid), &st))
			if st == "ok" {
				ok = true
				break
			}
		}
		if ok {
			log.Printf("题 %s 已提交 ✓", it.Pid)
		} else {
			log.Printf("题 %s 未确认到提交状态（可能仍在处理，继续下一题）", it.Pid)
			failed++
		}
	}
	log.Printf("完成：提交 %d 道，失败/未确认 %d 道", done-failed, failed)

	// 读取总分
	time.Sleep(2 * time.Second)
	var scoreStr string
	_ = safeRun(quizCtx, chromedp.Evaluate(quizScoreJS, &scoreStr))
	scoreFloat, _ := strconv.ParseFloat(strings.TrimSpace(scoreStr), 64)
	if scoreStr == "" {
		log.Printf("未能读取到总分")
	}
	return int(scoreFloat), nil
}

// dumpPage 把当前页面 HTML 保存到 page.html，用于确认真实 DOM 结构后精修选择器。
// 登录后的主页面是 JS 动态渲染的，先轮询等 body 真正有内容再 dump，
// 同时打印所有链接/按钮清单，帮助定位"做题/练习"入口。
func dumpPage(ctx context.Context) {
	dumpCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	// 等待动态内容渲染：轮询 body 文本长度，直到有实质内容（或 30s 超时）
	log.Printf("等待页面动态内容渲染...")
	for i := 0; i < 15; i++ {
		var bodyLen int
		_ = chromedp.Run(dumpCtx, chromedp.Evaluate(`document.body ? (document.body.innerText||'').trim().length : -1`, &bodyLen))
		if bodyLen > 30 {
			log.Printf("页面内容已渲染（正文 %d 字符）", bodyLen)
			break
		}
		time.Sleep(2 * time.Second)
	}

	// 直接执行 JS 取整页 HTML，避免 chromedp 的"等待元素"动作在 frameset 等
	// 老式页面结构上挂起而超时
	var html string
	if err := chromedp.Run(dumpCtx, chromedp.Evaluate(`document.documentElement.outerHTML`, &html)); err != nil {
		log.Printf("dump 页面失败: %v", err)
		return
	}

	// 诊断：打印当前 URL / 标题 / 长度，判断是否抓到了空壳页面
	var href, title string
	_ = chromedp.Run(dumpCtx, chromedp.Evaluate(`location.href`, &href))
	_ = chromedp.Run(dumpCtx, chromedp.Evaluate(`document.title`, &title))
	log.Printf("[诊断] dump 时页面 href=%s title=%q 长度=%d", href, title, len(html))

	// 打印所有链接和可见按钮文本，用于定位做题入口
	var navInfo string
	_ = chromedp.Run(dumpCtx, chromedp.Evaluate(`JSON.stringify((function(){
		var links = Array.from(document.querySelectorAll('a[href]')).filter(function(a){return a.offsetParent!==null;}).map(function(a){
			return {text:(a.innerText||'').trim().slice(0,30), href:(a.getAttribute('href')||'').slice(0,80)};
		}).filter(function(x){return x.text!=='';});
		var btns = Array.from(document.querySelectorAll('button, input[type=button], input[type=submit]')).filter(function(b){return b.offsetParent!==null;}).map(function(b){
			return (b.innerText||b.value||'').trim().slice(0,30);
		}).filter(function(t){return t!=='';});
		return {links:links, buttons:btns};
	})())`, &navInfo))
	log.Printf("[诊断] 页面导航元素: %s", navInfo)

	if err := os.WriteFile("page.html", []byte(html), 0644); err != nil {
		log.Printf("写 page.html 失败: %v", err)
		return
	}
	log.Printf("已保存页面结构到 page.html（%d 字节）", len(html))
}

// userDataDir 返回持久化的浏览器用户目录，登录态（Cookie）会存于此。
// 登录成功后 Cookie 持久化，之后运行可自动复用，不必每次输验证码。
func userDataDir() string {
	if d := os.Getenv("CHROME_USER_DATA"); d != "" {
		return d
	}
	return ".chrome-profile"
}
