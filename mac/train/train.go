package train

import (
	"bufio"
	"context"
	"cqupt/ai"
	"cqupt/config"
	"encoding/json"
	"fmt"
	"log/slog"
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

// runQuiz 启动一个浏览器实例，登录一次后串行刷题。
// 每次进入做题页后自动检测题型：编程题（Monaco）或单选填空题（整页多题滚动）。
//
// 注意：新站点有瑞数 WAF——它会检测"CDP 附加时的自动化副作用"并拒绝渲染页面
// （这就是之前一切"白屏/39 字节空壳"的根因）。验证过的可行流程是：
// 原生启动 Chrome（不附加任何自动化连接）→ 等 WAF 挑战自动通过 → 再附加 chromedp。
//
// 本函数不再是入口：对外的入口是 api.go 里的 Run。
func runQuiz(username, password string, num int, dump bool) (int, error) {
	if cdp := config.C.CDPURL; cdp != "" {
		// 高级模式：接入你自己已打开并登录的浏览器（需开启远程调试端口）
		slog.Info("已接入远程浏览器，假设已登录，跳过自动登录", "url", cdp)
		alloctx, cancelAlloc := chromedp.NewRemoteAllocator(context.Background(), cdp)
		defer cancelAlloc()
		ctx, cancelCtx := chromedp.NewContext(alloctx)
		defer cancelCtx()
		return solveLoop(ctx, num, dump)
	}

	// 1) 原生启动 Chrome：启动参数打开登录页的那个标签页全程无任何 CDP 附加，
	//    对 WAF 而言与真人打开无异，挑战可以自动通过。
	//    同时从 stderr 读取浏览器级 WebSocket 地址（"DevTools listening on ws://..."）。
	chromeCmd, browserWS, err := launchHumanChrome(config.C.CDPPort, config.UserDataDir(), config.URLLogin)
	if err != nil {
		return 0, err
	}
	defer func() {
		_ = chromeCmd.Process.Kill()
		_ = chromeCmd.Wait()
	}()

	// 到这里的 browserWS 一定非空：launchHumanChrome 读不到调试地址就直接报错了。
	// （旧版本会退回 http://127.0.0.1:port 兜底，那条路会接管到别人的浏览器、
	//   静默破坏 WAF 绕过的前提，已去掉。）
	alloctx, cancelAlloc := chromedp.NewRemoteAllocator(context.Background(), browserWS)
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
	slog.Info("等待 WAF 挑战自动通过（原生标签页无干扰加载中）",
		"约", config.WaitWAFChallenge.String())
	time.Sleep(config.WaitWAFChallenge)

	// 4) 我们的标签页导航到登录页：此时浏览器内已有合法 WAF cookie，
	//    服务器直接返回真实页面、不再下发挑战，也就不会触发 CDP 检测。
	if err := ensureLoginPage(ctx); err != nil {
		return 0, err
	}

	// 5) 自动登录：填学号密码 → 截图验证码 → ddddocr 识别 → 提交 → 校验，失败自动重试
	if err := autoLogin(ctx, username, password); err != nil {
		return 0, fmt.Errorf("登录失败: %w", err)
	}
	emit(Event{Kind: EventLoginOK, Detail: "已登录"})

	// 6) 登录后停在 main.jsp 作业卡片列表：选一张卡进入答题页
	//    （默认选标题含 ASSIGN_KEYWORD 的卡）
	//    dump 模式下若进不去，也照样把当前页 dump 出来方便排查
	quizCtx, cancelQuiz, err := enterAssignment(ctx, alloctx)
	if err != nil {
		if dump {
			slog.Warn("进入作业失败，直接 dump 当前页面", "err", err)
			dumpPage(ctx)
			return 0, nil
		}
		return 0, fmt.Errorf("进入作业失败: %w", err)
	}
	if cancelQuiz != nil {
		defer cancelQuiz()
	}
	emit(Event{Kind: EventAssignmentIn, Detail: "已进入答题页"})

	return solveLoop(quizCtx, num, dump)
}

// ensureLoginPage 导航到登录页并校验拿到的是真实页面（挑战未过时页面为空壳，等待重试）
func ensureLoginPage(ctx context.Context) error {
	for i := 1; i <= config.MaxLoginNavRetry; i++ {
		navCtx, cancel := context.WithTimeout(ctx, config.TimeoutLoginNav)
		err := safeRun(navCtx, chromedp.Navigate(config.URLLogin), chromedp.Sleep(5*time.Second))
		var title string
		var l int
		_ = safeRun(navCtx, chromedp.Evaluate(`document.title`, &title))
		_ = safeRun(navCtx, chromedp.Evaluate(`document.documentElement ? document.documentElement.outerHTML.length : -1`, &l))
		cancel()
		if err == nil && l > 2000 {
			slog.Info("登录页就绪", "title", title, "len", l)
			return nil
		}
		slog.Warn("登录页内容异常，挑战可能仍在进行，稍后重试",
			"len", l, "title", title, "err", err, "第几次", i, "共", config.MaxLoginNavRetry)
		time.Sleep(config.WaitLoginNavRetry)
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
				slog.Warn("编程题处理出错", "第几题", i+1, "err", err)
				continue
			}
			if finished {
				slog.Info("题目已全部完成，提前结束")
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

// ===================== 进入作业（登录后主页是作业卡片列表） =====================

// assignmentCardsJS 枚举 main.jsp 上的作业卡片：找文本含"进入作业/开始答题"的可见按钮，
// 向上爬取所属卡片的标题行（排除"作业"标签、作业时间、截止等行）。
// 返回 [{i, title, text, href}]。
const assignmentCardsJS = `(function(){
  var out = [];
  Array.from(document.querySelectorAll('` + config.SelAnyLinkButton + `')).forEach(function(b){
    var t = (b.innerText || '').trim();
    if (!/` + config.ReAssignmentBtn + `/.test(t)) return;
    if (b.offsetParent === null) return;
    var card = b;
    for (var k = 0; k < 8 && card.parentElement; k++) {
      card = card.parentElement;
      if ((card.innerText || '').length > 60) break;
    }
    var lines = (card.innerText || '').split('\n').map(function(s){ return s.trim(); })
      .filter(function(s){ return s !== '' && s !== '作业' && !/` + config.ReAssignmentNoise + `/.test(s); });
    out.push({i: out.length, title: (lines[0] || '').slice(0, 60), text: t, href: b.href || ''});
  });
  return JSON.stringify(out);
})()`

// assignmentClickJS 点击第 idx 张作业卡的"进入作业"按钮（与上面枚举同序）
func assignmentClickJS(idx int) string {
	return fmt.Sprintf(`(function(){
  var btns = [];
  Array.from(document.querySelectorAll('`+config.SelAnyLinkButton+`')).forEach(function(b){
    var t = (b.innerText || '').trim();
    if (/`+config.ReAssignmentBtn+`/.test(t) && b.offsetParent !== null) btns.push(b);
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
	for i := 0; i < config.PollAssignmentMax; i++ {
		pollCtx, cancel := context.WithTimeout(ctx, config.TimeoutAssignmentPoll)
		_ = safeRun(pollCtx, chromedp.Evaluate(assignmentCardsJS, &raw))
		cancel()
		if err := json.Unmarshal([]byte(raw), &cards); err == nil && len(cards) > 0 {
			found = true
			break
		}
		time.Sleep(config.PollAssignmentTick)
	}
	if !found {
		return nil, nil, fmt.Errorf("主页上没有找到任何'进入作业'按钮（可能页面未渲染，可 go run . -dump 查看）")
	}

	for _, c := range cards {
		slog.Info("发现作业卡", "序号", c.I, "标题", c.Title, "按钮", c.Text)
	}

	keyword := config.C.AssignKeyword
	choice := -1
	for _, c := range cards {
		if strings.Contains(c.Title, keyword) {
			choice = c.I
			break
		}
	}
	if choice < 0 {
		choice = cards[0].I
		slog.Warn("没有标题匹配的作业卡，默认进入第一张", "关键词", keyword, "提示", "可用 ASSIGN_KEYWORD 环境变量指定")
	}
	slog.Info("进入作业卡", "序号", choice, "标题", cards[choice].Title)

	// 记录点击前的 URL 和已有标签页，用于判断是同页跳转还是新开标签页
	var beforeHref string
	_ = safeRun(ctx, chromedp.Evaluate(`location.href`, &beforeHref))
	beforeTargets := map[target.ID]bool{}
	if ts, err := chromedp.Targets(ctx); err == nil {
		for _, t := range ts {
			beforeTargets[t.TargetID] = true
		}
	}

	clickCtx, cancelClick := context.WithTimeout(ctx, config.TimeoutAssignClick)
	var resp string
	_ = safeRun(clickCtx, chromedp.Evaluate(assignmentClickJS(choice), &resp))
	cancelClick()
	if resp != "ok" {
		return nil, nil, fmt.Errorf("点击'进入作业'失败: %s", resp)
	}

	// 等结果：同页跳转（URL 变化）或新开标签页（出现新 target）
	for i := 0; i < config.PollAssignResultMax; i++ {
		time.Sleep(config.PollAssignResultTick)

		// 情况 1：同页跳转
		var nowHref string
		pollCtx, cancel := context.WithTimeout(ctx, config.TimeoutSingleAction/2)
		_ = safeRun(pollCtx, chromedp.Evaluate(`location.href`, &nowHref))
		cancel()
		if nowHref != "" && nowHref != beforeHref && !strings.Contains(nowHref, config.URLMainPage) {
			slog.Info("已进入答题页（同页跳转）", "url", nowHref)
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
				slog.Info("已进入答题页（新标签页）", "url", t.URL)
				return newCtx, newCancel, nil
			}
		}
	}
	return nil, nil, fmt.Errorf("点击'进入作业'后页面没有变化（超时）")
}

// ===================== 浏览器启动 / WAF 探测 / 附加 =====================

// launchHumanChrome 以"真人方式"原生启动 Chrome：只开调试端口，不建立任何 CDP 会话。
// 瑞数 WAF 检测的是 CDP 附加时的自动化副作用，这样启动时页面加载与真人无异。
// 返回：进程句柄 + 浏览器级 WebSocket 地址（从 stderr 的 "DevTools listening on ws://..." 读到）。
//
// 启动前先做占用检查（见 preflight.go）；等不到调试地址时**直接报错**，
// 不再退回按端口直连——那条路会把别人的浏览器接管过来，静默破坏 WAF 绕过的前提。
func launchHumanChrome(port, profile, url string) (*exec.Cmd, string, error) {
	bin, err := config.FindChrome()
	if err != nil {
		return nil, "", err
	}
	if err := guardBrowserStartup(port, profile); err != nil {
		return nil, "", err
	}
	slog.Info("使用浏览器", "path", bin)
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
	case <-time.After(config.TimeoutChromeWSWait):
		// 等满这么久还没读到地址，说明我们起的这个实例没能成为调试端口的拥有者。
		// 最典型的成因是 profile 或端口已被另一个 Chrome 占着——新实例把 URL
		// 交给已有实例后自己退出，自然不会打印自己的 DevTools 地址。
		//
		// 先把证据收齐（端口上有没有人、profile 有没有活的持有者），再决定怎么报。
		probe := probeCDPPort(port)
		holder := profileLockHolder(profile)

		// 调用方只在成功时负责给这个进程收尸（defer Kill+Wait），
		// 所以这里必须自己杀，否则屏幕上会多留一个孤儿 Chrome。
		// 它已经没有任何用处：调试地址压根没露出来。
		_ = cmd.Process.Kill()
		_ = cmd.Wait()

		if probe.InUse || holder != "" {
			// 证据确凿：这是确定性的本地环境问题，重试不会有任何变化，
			// 所以套 ErrBrowserBusy，让服务端别再花重试配额。
			return nil, "", browserBusyError(port, profile, probe, holder)
		}
		// 没找到占用证据：是浏览器没能把调试端口暴露出来，原因不在占用。
		// 这条**不**套 ErrBrowserBusy——它仍可能是临时故障（比如浏览器
		// 启动瞬间崩了），服务端重试一次是合理的。
		return nil, "", fmt.Errorf("等待 %s 仍未读到浏览器的调试地址（端口 %s，profile %s）；"+
			"可能是浏览器没能启动，或当前运行环境不允许它暴露调试端口。"+
			"诊断命令：go run ./tools/cdpdiag -headless",
			config.TimeoutChromeWSWait, port, profile)
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

// autoLogin 自动完成登录：填学号密码 → 截图验证码 → ddddocr 识别 → 提交 → 校验结果，
// 失败自动刷新验证码重试；OCR 多次不过或 OCR 不可用时转人工兜底。
func autoLogin(ctx context.Context, username, password string) error {
	// 先判断是否已处于登录态：登录页被重定向（表单迟迟不出现 + URL 离开登录页）
	// 说明 Cookie 有效，直接跳过登录。
	for i := 0; i < config.PollExistingLoginMax; i++ {
		var hasForm bool
		var href string
		checkCtx, cancelCheck := context.WithTimeout(ctx, config.TimeoutSingleAction/2)
		_ = safeRun(checkCtx,
			chromedp.Evaluate(fmt.Sprintf(`!!document.querySelector(%q)`, config.SelUsername), &hasForm),
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
		time.Sleep(config.PollExistingLoginTick)
	}

	// 等登录表单就绪（附加后标签页可能还在自刷新）
	readyCtx, cancelReady := context.WithTimeout(ctx, config.TimeoutLoginFormReady)
	err := safeRun(readyCtx, chromedp.WaitReady(config.SelUsername, chromedp.ByQuery))
	cancelReady()
	if err != nil {
		return fmt.Errorf("等待登录表单就绪失败: %w", err)
	}

	for i := 1; i <= config.MaxLoginTry; i++ {
		if err := fillInput(ctx, config.SelUsername, username); err != nil {
			return err
		}
		if err := fillInput(ctx, config.SelPassword, password); err != nil {
			return err
		}

		// 截图验证码（所见即所得）
		var png []byte
		capCtx, cancelCap := context.WithTimeout(ctx, config.TimeoutCaptchaShot)
		err := chromedp.Run(capCtx, chromedp.Screenshot(config.SelCaptchaImg, &png, chromedp.NodeVisible, chromedp.ByQuery))
		cancelCap()
		if err != nil {
			slog.Warn("截图验证码失败，转人工登录", "err", err)
			return manualLoginFallback(ctx)
		}
		imgPath := filepath.Join(os.TempDir(), config.FileCaptchaTemp)
		if err := os.WriteFile(imgPath, png, 0644); err != nil {
			return fmt.Errorf("保存验证码图片失败: %w", err)
		}

		code, err := ocrCaptcha(imgPath)
		if err != nil {
			slog.Warn("验证码 OCR 失败，转人工登录", "err", err)
			return manualLoginFallback(ctx)
		}
		slog.Info("登录尝试", "第几次", i, "OCR 验证码", code)
		if err := fillInput(ctx, config.SelCaptchaCode, code); err != nil {
			return err
		}

		submitCtx, cancelSub := context.WithTimeout(ctx, config.TimeoutLoginSubmit)
		_ = chromedp.Run(submitCtx, chromedp.Click(config.SelLoginBtn, chromedp.ByQuery))
		cancelSub()

		ok, reason := waitLoginResult(ctx)
		if ok {
			fmt.Println(">>> 自动登录成功！")
			return nil
		}
		slog.Warn("登录未成功，刷新验证码后重试", "原因", reason)
		// 点击验证码图片换新码
		refreshCtx, cancelRef := context.WithTimeout(ctx, config.TimeoutSingleAction)
		_ = chromedp.Run(refreshCtx, chromedp.Click(config.SelCaptchaImg, chromedp.ByQuery))
		cancelRef()
		time.Sleep(config.WaitCaptchaRefresh)
	}
	// OCR 多次不过（可能验证码区分大小写），转人工兜底：浏览器是可见的，用户可直接输
	slog.Warn("OCR 连续失败，转人工兜底", "次数", config.MaxLoginTry)
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
	fillCtx, cancel := context.WithTimeout(ctx, config.TimeoutSingleAction)
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
func waitLoginResult(ctx context.Context) (bool, string) {
	deadline := time.Now().Add(time.Duration(config.PollLoginResultMax) * config.PollLoginResultTick)
	for time.Now().Before(deadline) {
		pollCtx, cancel := context.WithTimeout(ctx, config.TimeoutSingleAction/2)
		var href string
		var hasPwd bool
		_ = chromedp.Run(pollCtx, chromedp.Evaluate(`location.href`, &href))
		_ = chromedp.Run(pollCtx, chromedp.Evaluate(
			fmt.Sprintf(`!!document.querySelector(%q)`, config.SelPassword), &hasPwd))
		cancel()
		if strings.Contains(href, config.TextLoginErrFlag) {
			return false, "loginErr=1（验证码错误，或学号/密码错误）"
		}
		if !hasPwd && !strings.Contains(href, "loginErr") {
			return true, ""
		}
		time.Sleep(config.PollLoginResultTick)
	}
	return false, "等待登录结果超时"
}

// ocrCaptcha 调用 ddddocr 识别验证码图片。
// python 解释器由 config.FindPython 按平台探测（优先隔离 venv，其次系统命令）。
func ocrCaptcha(imgPath string) (string, error) {
	py, err := config.FindPython()
	if err != nil {
		return "", err
	}
	script := filepath.Join(config.DirOcrScript, config.FileOcrScript)
	out, err := exec.Command(py, script, imgPath).CombinedOutput()
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
//
// 服务端（AllowManualLogin=false）会直接失败退出。这不是功能缺失，而是刻意的：
// 无人值守的 worker 一旦停在这里等人，就会一直占着锁，把队列里后面所有任务
// 一起堵死——单点长阻塞比快速失败糟糕得多。让任务失败、把原因写进数据库，
// 由人来决定是否重试，才是服务该有的样子。
func manualLoginFallback(ctx context.Context) error {
	if !curOpts.AllowManualLogin {
		emit(Event{Kind: EventManualLogin, Detail: "需要人工登录但当前为无人值守模式"})
		return ErrManualLoginRequired
	}
	fmt.Println(">>> 请在浏览器里手动输入学号、密码、验证码并登录。")
	fmt.Println(">>> 登录完成后脚本会自动检测继续；也可以在此按【回车】继续。")
	manualCh := make(chan struct{})
	go func() {
		stdin.ReadString('\n')
		close(manualCh)
	}()
	deadline := time.Now().Add(config.TimeoutManualLogin)
	seenForm := false
	ticker := time.NewTicker(config.PollManualLoginTick)
	defer ticker.Stop()
	for {
		pollCtx, cancel := context.WithTimeout(ctx, config.TimeoutNavPollTick)
		var hasPwd bool
		_ = chromedp.Run(pollCtx, chromedp.Evaluate(
			fmt.Sprintf(`!!document.querySelector(%q)`, config.SelPassword), &hasPwd))
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
				return fmt.Errorf("等待登录超时")
			}
		}
	}
}

// detectMode 检测当前做题页类型：code（Monaco 编程题）或 quiz（整页单选填空题）
func detectMode(ctx context.Context) string {
	modeCtx, cancel := context.WithTimeout(ctx, config.TimeoutSingleAction)
	defer cancel()
	// 等页面渲染稳定后再判断
	_ = chromedp.Run(modeCtx, chromedp.Sleep(config.PollAssignmentTick))

	var isCode, isQuiz bool
	_ = chromedp.Run(modeCtx,
		chromedp.Evaluate(`!!(window.monaco && monaco.editor && monaco.editor.getEditors && monaco.editor.getEditors().length > 0)`, &isCode),
		chromedp.Evaluate(`(function(){
			var t = document.body.innerText || '';
			if (/`+config.TextQuizMarkers+`/.test(t)) return true;
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
	mainCtx, cancel := context.WithTimeout(ctx, config.TimeoutCodeQuestion)
	defer cancel()

	// 未刷完，获取题干
	var question string
	if err = chromedp.Run(mainCtx,
		chromedp.Text(config.SelCodeQuestion, &question, chromedp.ByQuery),
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
		chromedp.WaitVisible(config.SelMonaco, chromedp.ByQuery),
		chromedp.Evaluate(fillCodeJS, nil),
		chromedp.WaitVisible(config.XPathSubmitBtn, chromedp.BySearch),
		chromedp.Click(config.XPathSubmitBtn, chromedp.BySearch),
		chromedp.WaitVisible(config.XPathConfirmBtn, chromedp.BySearch),
		chromedp.Click(config.XPathConfirmBtn, chromedp.BySearch),
		chromedp.WaitVisible(config.SelScoreCell),
		chromedp.Text(config.SelScoreCell, &scoreStr),
	); err != nil {
		return 0, false, fmt.Errorf("填充代码或提交失败: %w", err)
	}

	scoreInt, _ := strconv.Atoi(strings.TrimSpace(scoreStr))
	return scoreInt, false, nil
}

// ===================== 整页选择/填空题 =====================

// quizListJS 枚举页面上所有内嵌题目表单（真实 DOM：form[name=answerForm{题号}]）。
// 两种题干结构统一处理：选择题在 div[id^=tts{pid}text] 里、cloze 填空题直接是 form 内 <p>——
// 都包含在 form 内，所以统一克隆 form、去掉输入控件后取 innerText。
// 返回 [{pid, text, n, answered}]。
const quizListJS = `(function(){
  var out = [];
  document.querySelectorAll('` + config.SelQuizForm + `').forEach(function(f){
    var m = f.name.match(/` + config.ReQuizPid + `/);
    if (!m) return;
    var pid = m[1];
    var c = f.cloneNode(true);
    c.querySelectorAll('input,button,select,textarea,script').forEach(function(e){ e.remove(); });
    var text = (c.innerText || '').trim().slice(0, 1500);
    var inputs = f.querySelectorAll('` + config.SelQuizInput + `');
    var answered = false;
    inputs.forEach(function(inp){ if ((inp.value||'').trim() !== '') answered = true; });
    var tip = document.getElementById('` + config.SelQuizSaveTipPrefix + `'+pid);
    if (tip && (tip.innerText||'').indexOf('` + config.TextQuizSubmitted + `') >= 0) answered = true;
    if (inputs.length === 0) return;
    out.push({pid: pid, text: text, n: inputs.length, answered: answered});
  });
  return JSON.stringify(out);
})()`

// quizFillJS 填入第 pid 题的答案并触发自动提交：
// 全部输入框用原生 setter 填好值后，在最后一个 input 上派发 input + change 事件
// （选择题 oninput=form.submit()；cloze 填空 oninput=cgClozeInput + onchange=cgClozeChange，
// 提交到隐藏 iframe frame_problemhandler，页面不刷新）。
func quizFillJS(pid string, answers []string) string {
	ansBytes, _ := json.Marshal(answers)
	return fmt.Sprintf(`(function(){
  var f = document.forms['answerForm%[1]s'];
  if (!f) return 'no-form';
  var inputs = Array.from(f.querySelectorAll('`+config.SelQuizInput+`'));
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

// quizTipJS 读取第 pid 题的状态提示原文（可能形如"已提交"，若有分数也会一并带出）
func quizTipJS(pid string) string {
	return fmt.Sprintf(`(function(){
  var tip = document.getElementById('%s' + %q);
  return tip ? (tip.innerText || '').trim() : '';
})()`, config.SelQuizSaveTipPrefix, pid)
}

// quizScoreJS 从页面读取 "总分: x.xx"（读不到返回空串）
const quizScoreJS = `(function(){
  var m = (document.body.innerText || '').match(/` + config.ReTotalScoreJS + `/);
  return m ? m[1] : '';
})()`

// readTotalScore 读取页面总分；读不到返回 -1。
//
// 返回值语义：>= 0 是真实分数；-1 表示本页没有总分区域（例如题目页而非作业页）。
// 调用方必须先判断是否为负，否则会把"读不到"误当成"得了 0 分"。
func readTotalScore(ctx context.Context) float64 {
	var s string
	if err := safeRun(ctx, chromedp.Evaluate(quizScoreJS, &s)); err != nil {
		return -1
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return -1
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return -1
	}
	return f
}

// waitScoreChange 提交后等总分发生变化（服务器算分需要时间），
// 返回新总分；若在等待窗口内没变，返回 before 本身。
func waitScoreChange(ctx context.Context, before float64) float64 {
	deadline := time.Now().Add(config.WaitScoreChange)
	for time.Now().Before(deadline) {
		time.Sleep(config.PollScoreTick)
		now := readTotalScore(ctx)
		if now >= 0 && now != before {
			return now
		}
	}
	return before
}

// quizItem 是一道内嵌题的元信息
type quizItem struct {
	Pid      string `json:"pid"`
	Text     string `json:"text"`
	N        int    `json:"n"`
	Answered bool   `json:"answered"`
}

// genQuizAnswers 生成某道题的答案；try > 1 时带上重答提醒。
func genQuizAnswers(it quizItem, try int) ([]string, error) {
	hint := ""
	if try > 1 {
		hint = ai.Prompts.ReanswerSuffix
	}
	if it.N == 1 {
		letter, err := ai.AnswerChoice(it.Text, hint)
		if err != nil {
			return nil, err
		}
		return []string{letter}, nil
	}
	return ai.AnswerMulti(it.Text, it.N, hint)
}

// solveQuizItem 作答单道题，含"提交后回读得分 → 不达标则重答"的闭环。
//
// 返回该题的最终得分；-1 表示"得分无法判定"（页面没有总分区域等），
// 这种情况下不做重答，避免把读不到分数误判成答错。
func solveQuizItem(ctx context.Context, it quizItem) float64 {
	lastScore := -1.0
	var lastAnswers []string
	for try := 1; try <= config.C.MaxAnswerTry; try++ {
		before := readTotalScore(ctx)

		answers, err := genQuizAnswers(it, try)
		if err != nil {
			slog.Error("生成答案失败", "pid", it.Pid, "err", err)
			return lastScore
		}
		lastAnswers = answers
		if try > 1 {
			slog.Info("重答中", "pid", it.Pid, "第几次", try, "上次得分", lastScore)
		}

		var resp string
		if err := safeRun(ctx, chromedp.Evaluate(quizFillJS(it.Pid, answers), &resp)); err != nil || resp != "ok" {
			slog.Error("填入答案失败", "pid", it.Pid, "resp", resp, "err", err)
			return lastScore
		}

		// 等自动提交生效（saveTip 变"已提交"）
		submitted := false
		for i := 0; i < config.PollQuizSubmittedMax; i++ {
			time.Sleep(config.PollQuizSubmittedTick)
			var tip string
			_ = safeRun(ctx, chromedp.Evaluate(quizTipJS(it.Pid), &tip))
			if strings.Contains(tip, config.TextQuizSubmitted) {
				submitted = true
				break
			}
		}
		if !submitted {
			slog.Warn("未确认到提交状态（可能仍在处理）", "pid", it.Pid)
			return lastScore
		}

		// 回读得分：用提交前后的总分差值，得到"这一题贡献了多少分"
		if before < 0 {
			// 页面没有总分区域，无法判定得分 —— 保持改造前的行为（提交完就往下走）
			slog.Info("题目已提交（本页无总分区域，跳过得分回读）", "pid", it.Pid)
			noteResult(QuestionResult{
				Pid: it.Pid, Text: it.Text, Answer: strings.Join(lastAnswers, " | "),
				Score: -1, Tries: try,
				Judgment: "本页无总分区域，得分无法判定（按通过处理，不重答）",
			})
			return -1
		}
		after := waitScoreChange(ctx, before)
		delta := after - before
		if delta < 0 {
			delta = 0
		}
		lastScore = delta

		if !config.C.ShouldReanswer(delta, try) {
			if delta > 0 {
				slog.Info("题目已提交并得分", "pid", it.Pid, "得分", delta, "总分", after)
			} else {
				slog.Info("题目已提交", "pid", it.Pid, "得分", delta, "总分", after)
			}
			noteResult(QuestionResult{
				Pid: it.Pid, Text: it.Text, Answer: strings.Join(lastAnswers, " | "),
				Score: delta, Tries: try,
				Judgment: fmt.Sprintf("第 %d 次作答得 %g 分，页面总分 %g", try, delta, after),
			})
			return lastScore
		}
		slog.Warn("本次得分未达阈值，准备重答", "pid", it.Pid, "得分", delta, "已尝试", try, "上限", config.C.MaxAnswerTry)
	}

	// 次数用尽仍未达标：记录到错题文件，便于事后复盘
	recordWrongAnswer(it, lastScore)
	noteResult(QuestionResult{
		Pid: it.Pid, Text: it.Text, Answer: strings.Join(lastAnswers, " | "),
		Score: lastScore, Tries: config.C.MaxAnswerTry,
		Judgment: fmt.Sprintf("已尝试 %d 次仍未达标", config.C.MaxAnswerTry),
	})
	return lastScore
}

// solveQuizPage 处理主页上的整卷选择/填空题（最多做 num 道未提交的）：
// 等页面渲染 -> 枚举题目 -> 逐题 AI 作答并填入 -> 校验已提交 -> 回读得分 -> 读总分
func solveQuizPage(ctx context.Context, num int) (int, error) {
	quizCtx, cancel := context.WithTimeout(ctx, config.TimeoutQuizPage)
	defer cancel()

	// 等动态渲染：轮询直到题目表单出现
	var raw string
	var items []quizItem
	for i := 0; i < config.PollQuizListMax; i++ {
		if err := safeRun(quizCtx, chromedp.Evaluate(quizListJS, &raw)); err != nil {
			return 0, fmt.Errorf("读取题目列表失败: %w", err)
		}
		if err := json.Unmarshal([]byte(raw), &items); err != nil {
			return 0, fmt.Errorf("解析题目列表失败: %w", err)
		}
		if len(items) > 0 {
			break
		}
		time.Sleep(config.PollQuizListTick)
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
	slog.Info("题目枚举完成", "总数", len(items), "未提交", unanswered, "本次最多做", num)

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

		s := solveQuizItem(ctx, it)
		switch {
		case s < 0:
			slog.Info("题目已处理（本页无法判定得分）", "pid", it.Pid)
		case s == 0:
			slog.Warn("题目已提交但未得分", "pid", it.Pid)
			failed++
		default:
			slog.Info("题目已得分", "pid", it.Pid, "得分", s)
		}
	}
	slog.Info("完成", "本轮处理", done, "疑似失分", failed)

	// 读取总分
	time.Sleep(config.WaitBeforeReadScore)
	total := readTotalScore(quizCtx)
	if total < 0 {
		slog.Warn("未能读取到总分")
		return 0, nil
	}
	slog.Info("页面总分", "score", total)
	return int(total), nil
}

// recordWrongAnswer 把一道反复答不对的题写入错题文件。
// 只做记录、不中断流程——刷题是批量任务，中途停下来反而更糟。
func recordWrongAnswer(it quizItem, score float64) {
	f, err := os.OpenFile(config.FileWrongAnswers, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		slog.Warn("写入错题文件失败", "err", err)
		return
	}
	defer f.Close()

	ts := time.Now().Format("2006-01-02 15:04:05")
	var b strings.Builder
	fmt.Fprintf(&b, "\n## 题 %s（%s，得分 %g）\n\n", it.Pid, ts, score)
	fmt.Fprintf(&b, "**空数**：%d ｜ **已尝试**：%d 次\n\n", it.N, config.C.MaxAnswerTry)
	fmt.Fprintf(&b, "**题干**：\n\n```\n%s\n```\n", it.Text)
	if _, err := f.WriteString(b.String()); err != nil {
		slog.Warn("写入错题文件失败", "err", err)
		return
	}
	slog.Warn("已记入错题文件，便于事后复盘", "file", config.FileWrongAnswers, "pid", it.Pid)
}

// dumpPage 把当前页面 HTML 保存到 page.html，用于确认真实 DOM 结构后精修选择器。
// 登录后的主页面是 JS 动态渲染的，先轮询等 body 真正有内容再 dump，
// 同时打印所有链接/按钮清单，帮助定位"做题/练习"入口。
func dumpPage(ctx context.Context) {
	dumpCtx, cancel := context.WithTimeout(ctx, config.TimeoutDump)
	defer cancel()

	// 等待动态内容渲染：轮询 body 文本长度，直到有实质内容
	slog.Info("等待页面动态内容渲染...")
	for i := 0; i < config.PollDumpMax; i++ {
		var bodyLen int
		_ = chromedp.Run(dumpCtx, chromedp.Evaluate(`document.body ? (document.body.innerText||'').trim().length : -1`, &bodyLen))
		if bodyLen > config.RenderTextThreshold {
			slog.Info("页面内容已渲染", "正文字符数", bodyLen)
			break
		}
		time.Sleep(config.PollRenderTick)
	}

	// 直接执行 JS 取整页 HTML，避免 chromedp 的"等待元素"动作在 frameset 等
	// 老式页面结构上挂起而超时
	var html string
	if err := chromedp.Run(dumpCtx, chromedp.Evaluate(`document.documentElement.outerHTML`, &html)); err != nil {
		slog.Error("dump 页面失败", "err", err)
		return
	}

	// 诊断：打印当前 URL / 标题 / 长度，判断是否抓到了空壳页面
	var href, title string
	_ = chromedp.Run(dumpCtx, chromedp.Evaluate(`location.href`, &href))
	_ = chromedp.Run(dumpCtx, chromedp.Evaluate(`document.title`, &title))
	slog.Info("[诊断] dump 结果", "href", href, "title", title, "长度", len(html))

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
	slog.Info("[诊断] 页面导航元素", "nav", navInfo)

	if err := os.WriteFile(config.FileDumpQuiz, []byte(html), 0644); err != nil {
		slog.Error("写 dump 文件失败", "file", config.FileDumpQuiz, "err", err)
		return
	}
	slog.Info("已保存页面结构", "file", config.FileDumpQuiz, "字节", len(html))
}
