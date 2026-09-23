package main

// ============================================================================
// progap.go —— 程序片段编程题（programFillGapList.jsp）答题支持
//
// 设计原则：
//  1. 全部逻辑在本文件内自成一派，不修改 main.go 里已经跑通的整页刷题（quiz）逻辑；
//     main.go 只在入口加了一个 -mode 开关分支（默认 quiz，行为与之前完全一致）。
//  2. 复用 main.go 里已有的通用件：launchHumanChrome / ensureLoginPage /
//     autoLogin / enterAssignment（WAF 绕过、OCR 登录、选作业卡），不重复造轮子。
//  3. 程序题答题页的 DOM 结构未知，所以本文件内含"结构探测 + 自动 dump"：
//     处理第一题时会把页面存成 progap.html 并打印页面结构（编辑器类型、
//     输入框数量、按钮清单），据此可以精确调整选择器。
// ============================================================================

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"main/ai"
	"os"
	"strings"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/cloudwego/eino/schema"
)

// modeFlag 运行模式：
//
//	quiz  （默认）整页选择填空题，走 main.go 原有逻辑
//	progap     程序片段编程题，自动作答
//	progapdump 只进入第一道程序题并把页面存成 progap.html（用于调选择器）
var modeFlag = flag.String("mode", "quiz", "运行模式：quiz=整页选择填空（默认）；progap=程序片段编程题；progapdump=只 dump 程序题页面")

// runProgap 程序题完整流程：启动浏览器 → 登录 → 进入作业 → 作答。
// dumpOnly=true 时只 dump 第一道程序题页面后返回。
func runProgap(username, password string, num int, dumpOnly bool) (int, error) {
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

	allocURL := "http://127.0.0.1:" + port
	if browserWS != "" {
		allocURL = browserWS
	}
	alloctx, cancelAlloc := chromedp.NewRemoteAllocator(context.Background(), allocURL)
	defer cancelAlloc()

	ctx, cancelCtx := chromedp.NewContext(alloctx)
	defer cancelCtx()
	if err := safeRun(ctx); err != nil {
		return 0, fmt.Errorf("建立浏览器控制连接失败: %w", err)
	}

	log.Printf("等待 WAF 挑战自动通过（约 20 秒）...")
	time.Sleep(20 * time.Second)

	if err := ensureLoginPage(ctx); err != nil {
		return 0, err
	}
	if err := autoLogin(ctx, username, password); err != nil {
		return 0, fmt.Errorf("登录失败: %w", err)
	}

	quizCtx, cancelQuiz, err := enterAssignment(ctx, alloctx)
	if err != nil {
		return 0, fmt.Errorf("进入作业失败: %w", err)
	}
	if cancelQuiz != nil {
		defer cancelQuiz()
	}

	if dumpOnly {
		return 0, progapDumpFirst(quizCtx)
	}
	return progapSolve(quizCtx, num)
}

// ===================== 列表枚举 =====================

// progapListJS 枚举作业页上"程序片段编程题"表格里的题目行。
// 真实结构：<tr><th>1.</th><td><a href="/assignment/programFillGapList.jsp?proNum=N&assignID=X">标题</a></td>
//
//	<td>15.00</td><td><small>还未提交答案</small>...</td></tr>
//
// 返回 [{i, title, href, status, done}]，done=true 表示已经提交过（跳过）。
const progapListJS = `(function(){
  var out = [];
  Array.from(document.querySelectorAll('a[href*="programFillGapList"]')).forEach(function(a){
    if (a.offsetParent === null) return;
    var tr = a.closest('tr');
    var status = '';
    if (tr) {
      var cells = Array.from(tr.querySelectorAll('td,th')).map(function(c){ return (c.innerText||'').trim(); });
      status = cells[cells.length-1] || '';
    }
    var done = !/还未提交|未提交/.test(status);
    out.push({i: out.length, title: (a.innerText||'').trim().slice(0,80), href: a.href, status: status.slice(0,40), done: done});
  });
  return JSON.stringify(out);
})()`

type progapItem struct {
	I      int    `json:"i"`
	Title  string `json:"title"`
	Href   string `json:"href"`
	Status string `json:"status"`
	Done   bool   `json:"done"`
}

// progapEnumerate 读取当前作业页上的程序题列表（最多等 40s 渲染）
func progapEnumerate(ctx context.Context) ([]progapItem, error) {
	var raw string
	var items []progapItem
	for i := 0; i < 20; i++ {
		pollCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
		_ = safeRun(pollCtx, chromedp.Evaluate(progapListJS, &raw))
		cancel()
		if err := json.Unmarshal([]byte(raw), &items); err == nil && len(items) > 0 {
			return items, nil
		}
		time.Sleep(2 * time.Second)
	}
	return nil, fmt.Errorf("作业页上没有找到程序片段编程题（programFillGapList 链接）")
}

// ===================== 答题页结构探测 =====================

// progapProbeJS 探测程序题答题页结构：编辑器类型、输入框数量、按钮清单、正文内容。
const progapProbeJS = `(function(){
  function txt(el){ return el ? (el.innerText||'').trim() : ''; }
  var editors = [];
  try {
    if (window.monaco && monaco.editor && monaco.editor.getEditors) editors.push('monaco:' + monaco.editor.getEditors().length);
  } catch(e) {}
  var cm = document.querySelector('.CodeMirror');
  if (cm && cm.CodeMirror) editors.push('codemirror');
  var tas = Array.from(document.querySelectorAll('textarea')).filter(function(t){return t.offsetParent!==null;});
  if (tas.length) editors.push('textarea:' + tas.length);
  var inputs = Array.from(document.querySelectorAll('input')).filter(function(i){
    return i.offsetParent !== null && i.type !== 'hidden' && i.type !== 'radio' && i.type !== 'checkbox';
  });
  var btns = Array.from(document.querySelectorAll('button, input[type=button], input[type=submit], a.btn'))
    .filter(function(b){ return b.offsetParent !== null; })
    .map(function(b){ return (txt(b) || b.value || '').slice(0, 20); })
    .filter(function(t){ return t !== ''; });
  // 正文：优先取最大的内容容器，兜底 body
  var body = txt(document.body).slice(0, 6000);
  var blanks = (body.match(/_ {0,2}_{2,}|【.*?】|\[\s*\]/g) || []).length;
  return JSON.stringify({editors: editors, inputs: inputs.length, buttons: btns.slice(0, 20), body: body, blanks: blanks});
})()`

type progapProbeInfo struct {
	Editors []string `json:"editors"`
	Inputs  int      `json:"inputs"`
	Buttons []string `json:"buttons"`
	Body    string   `json:"body"`
	Blanks  int      `json:"blanks"`
}

func progapProbe(ctx context.Context) (progapProbeInfo, error) {
	var p progapProbeInfo
	var raw string
	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := safeRun(probeCtx, chromedp.Evaluate(progapProbeJS, &raw)); err != nil {
		return p, err
	}
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return p, fmt.Errorf("解析页面结构失败: %w", err)
	}
	return p, nil
}

// progapDumpFirst 进入第一道程序题并保存页面（用于精修选择器）
func progapDumpFirst(ctx context.Context) error {
	items, err := progapEnumerate(ctx)
	if err != nil {
		return err
	}
	log.Printf("共 %d 道程序题，dump 第一道：%s (%s)", len(items), items[0].Title, items[0].Href)

	navCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if err := safeRun(navCtx, chromedp.Navigate(items[0].Href)); err != nil {
		return err
	}
	// 等页面渲染
	for i := 0; i < 15; i++ {
		var l int
		_ = safeRun(navCtx, chromedp.Evaluate(`(document.body.innerText||'').trim().length`, &l))
		if l > 100 {
			break
		}
		time.Sleep(2 * time.Second)
	}

	var html string
	_ = safeRun(navCtx, chromedp.Evaluate(`document.documentElement.outerHTML`, &html))
	if err := os.WriteFile("progap.html", []byte(html), 0644); err != nil {
		log.Printf("写 progap.html 失败: %v", err)
	} else {
		log.Printf("已保存程序题页面到 progap.html（%d 字节）", len(html))
	}

	p, err := progapProbe(ctx)
	if err != nil {
		return err
	}
	log.Printf("[结构] 编辑器=%v 可见输入框=%d 疑似空格数=%d 按钮=%v", p.Editors, p.Inputs, p.Blanks, p.Buttons)
	log.Printf("[结构] 正文前 1500 字:\n%s", truncate(p.Body, 1500))
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// ===================== 作答 =====================

// progapQuestionJS 提取程序题题干（基于真实 DOM）：
// 标题 + #cgsoucecode 代码区，把嵌在代码块之间的 textarea 替换成"【空N】"占位符后取文本。
// 这样 AI 能看到完整上下文：上方代码 → 空 → 下方代码。
const progapQuestionJS = `(function(){
  var root = document.getElementById('cgsoucecode') || document.querySelector('form[name="uploadFORM"]') || document.body;
  var title = '';
  var h = document.querySelector('h1,h2,h3,h4,h5,h6');
  if (h) title = (h.innerText || '').trim();
  var c = root.cloneNode(true);
  var tas = c.querySelectorAll('textarea');
  tas.forEach(function(t, i){
    var marker = document.createElement('span');
    marker.textContent = '\n【空' + (i+1) + '：在此填入代码】\n';
    t.parentNode.replaceChild(marker, t);
  });
  c.querySelectorAll('script,style').forEach(function(e){ e.remove(); });
  return JSON.stringify({
    title: title,
    text: (c.innerText || '').trim().slice(0, 5000),
    blanks: tas.length
  });
})()`

// progapFillAnswersJS 按顺序把答案填入 name=answerN 的 textarea（原生 setter + 事件）
func progapFillAnswersJS(answers []string) string {
	ansBytes, _ := json.Marshal(answers)
	return fmt.Sprintf(`(function(){
  var tas = Array.from(document.querySelectorAll('textarea[name^="answer"]'));
  var answers = %s;
  if (tas.length !== answers.length) return 'count-mismatch:' + tas.length;
  var set = Object.getOwnPropertyDescriptor(window.HTMLTextAreaElement.prototype, 'value').set;
  for (var i = 0; i < tas.length; i++) {
    tas[i].scrollIntoView({block: 'center'});
    set.call(tas[i], answers[i] || '');
    tas[i].dispatchEvent(new Event('input', {bubbles: true}));
    tas[i].dispatchEvent(new Event('change', {bubbles: true}));
  }
  return 'ok';
})()`, string(ansBytes))
}

// progapResultJS 读取提交结果 iframe（showmessageFRAME）的文本
const progapResultJS = `(function(){
  var f = document.querySelector('iframe[name="showmessageFRAME"]');
  if (!f || !f.contentDocument || !f.contentDocument.body) return '';
  return (f.contentDocument.body.innerText || '').trim().slice(0, 1000);
})()`

// progapSolve 逐题作答：进入题目 → 探测结构 → AI 生成 → 填入 → 提交 → 回列表校验状态
func progapSolve(ctx context.Context, num int) (int, error) {
	items, err := progapEnumerate(ctx)
	if err != nil {
		return 0, err
	}
	var listURL string
	_ = safeRun(ctx, chromedp.Evaluate(`location.href`, &listURL))

	todo := 0
	for _, it := range items {
		if !it.Done {
			todo++
		}
	}
	log.Printf("程序片段编程题共 %d 道，其中 %d 道未提交，本次最多做 %d 道", len(items), todo, num)

	done, failed := 0, 0
	for _, it := range items {
		if done >= num {
			break
		}
		if it.Done {
			continue
		}
		done++
		fmt.Printf("[%d] %s 解答中...\n", done, it.Title)

		if err := progapAnswerOne(ctx, it, done == 1); err != nil {
			log.Printf("题 %q 失败: %v", it.Title, err)
			failed++
			// 回到列表页，继续下一题
			backCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			_ = safeRun(backCtx, chromedp.Navigate(listURL))
			cancel()
			continue
		}
		log.Printf("题 %q 已提交 ✓", it.Title)

		backCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		_ = safeRun(backCtx, chromedp.Navigate(listURL))
		cancel()
		time.Sleep(2 * time.Second)
	}
	log.Printf("完成：提交 %d 道，失败 %d 道", done-failed, failed)
	return done - failed, nil
}

// progapAnswerOne 处理单道程序题（按真实 DOM：textarea 嵌在代码块之间，逐空作答）
func progapAnswerOne(ctx context.Context, it progapItem, dumpFirst bool) error {
	oneCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	if err := safeRun(oneCtx, chromedp.Navigate(it.Href)); err != nil {
		return fmt.Errorf("打开题目失败: %w", err)
	}
	// 等渲染（textarea 出现才算就绪）
	for i := 0; i < 15; i++ {
		var n int
		_ = safeRun(oneCtx, chromedp.Evaluate(`document.querySelectorAll('textarea[name^="answer"]').length`, &n))
		if n > 0 {
			break
		}
		time.Sleep(2 * time.Second)
	}

	// 提取题干：标题 + 代码区（textarea 替换为【空N】占位符）
	var qraw string
	var q struct {
		Title  string `json:"title"`
		Text   string `json:"text"`
		Blanks int    `json:"blanks"`
	}
	if err := safeRun(oneCtx, chromedp.Evaluate(progapQuestionJS, &qraw)); err != nil {
		return fmt.Errorf("提取题干失败: %w", err)
	}
	if err := json.Unmarshal([]byte(qraw), &q); err != nil {
		return fmt.Errorf("解析题干失败: %w", err)
	}
	if q.Blanks == 0 {
		// 兜底：题目结构不是"代码内嵌 textarea"（可能是整段编程题），走通用分支
		log.Printf("  未发现填空 textarea，走通用编辑器分支")
		return progapAnswerOneGeneric(oneCtx)
	}
	log.Printf("  题干: %s（%d 个空）", q.Title, q.Blanks)

	// 第一题自动保存页面 + 打印题干，便于核对
	if dumpFirst {
		var html string
		_ = safeRun(oneCtx, chromedp.Evaluate(`document.documentElement.outerHTML`, &html))
		_ = os.WriteFile("progap.html", []byte(html), 0644)
		log.Printf("  已保存第一道程序题页面到 progap.html（%d 字节）", len(html))
		log.Printf("  题干前 600 字: %s", truncate(q.Text, 600))
	}

	// AI 逐空生成答案
	question := q.Title + "\n" + q.Text
	answers, err := aiProgapBlanks(question, q.Blanks)
	if err != nil {
		return err
	}

	// 填入 textarea
	var resp string
	if err := safeRun(oneCtx, chromedp.Evaluate(progapFillAnswersJS(answers), &resp)); err != nil || resp != "ok" {
		return fmt.Errorf("填入答案失败: %v %s", err, resp)
	}
	log.Printf("  已填入 %d 个空", len(answers))

	// 提交（#cgSubmitBtn，提交到隐藏 iframe，页面不刷新）
	return progapSubmitAndWait(oneCtx)
}

// progapSubmitAndWait 点 #cgSubmitBtn 提交，轮询结果 iframe 等判题结果
func progapSubmitAndWait(ctx context.Context) error {
	clickCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	err := safeRun(clickCtx, chromedp.Click(`#cgSubmitBtn`, chromedp.ByQuery, chromedp.NodeVisible))
	cancel()
	if err != nil {
		return fmt.Errorf("点击提交按钮失败: %w", err)
	}
	log.Printf("  已提交，等待判题结果...")

	// 判题是服务器跑测试用例，可能几秒到几十秒
	for i := 0; i < 30; i++ {
		time.Sleep(2 * time.Second)
		var result string
		_ = safeRun(ctx, chromedp.Evaluate(progapResultJS, &result))
		if result != "" && !strings.Contains(result, "正在") && !strings.Contains(result, "处理中") && !strings.Contains(result, "Loading") {
			log.Printf("  判题结果: %s", truncate(result, 300))
			return nil
		}
	}
	log.Printf("  未等到判题结果（可能服务器较慢），按已提交处理")
	return nil
}

// progapAnswerOneGeneric 通用兜底分支：整段代码编辑器或逐空 input（结构未知的题目）
func progapAnswerOneGeneric(ctx context.Context) error {
	p, err := progapProbe(ctx)
	if err != nil {
		return err
	}
	log.Printf("  结构: 编辑器=%v 输入框=%d 空格=%d 按钮=%v", p.Editors, p.Inputs, p.Blanks, p.Buttons)
	if p.Body == "" {
		return fmt.Errorf("页面正文为空，可能未正确加载")
	}

	isEditor := false
	for _, e := range p.Editors {
		if strings.HasPrefix(e, "monaco") || strings.HasPrefix(e, "codemirror") || strings.HasPrefix(e, "textarea") {
			isEditor = true
		}
	}

	if isEditor && p.Inputs == 0 {
		code, err := aiProgapCode(p.Body)
		if err != nil {
			return err
		}
		if err := progapFillEditor(ctx, code); err != nil {
			return err
		}
	} else if p.Inputs > 0 {
		answers, err := aiProgapBlanks(p.Body, p.Inputs)
		if err != nil {
			return err
		}
		if err := progapFillInputs(ctx, answers); err != nil {
			return err
		}
	} else {
		return fmt.Errorf("未识别到任何输入控件（编辑器=%v），已保存 progap.html 供排查", p.Editors)
	}
	return progapSubmit(ctx)
}

// progapFillEditor 把整段代码写入编辑器（Monaco / CodeMirror / textarea 三种都试）
func progapFillEditor(ctx context.Context, code string) error {
	codeBytes, _ := json.Marshal(code)
	js := fmt.Sprintf(`(function(){
  var code = %s;
  try {
    if (window.monaco && monaco.editor && monaco.editor.getEditors && monaco.editor.getEditors().length) {
      monaco.editor.getEditors()[0].setValue(code); return 'monaco';
    }
  } catch(e) {}
  var cm = document.querySelector('.CodeMirror');
  if (cm && cm.CodeMirror) { cm.CodeMirror.setValue(code); return 'codemirror'; }
  var tas = Array.from(document.querySelectorAll('textarea')).filter(function(t){return t.offsetParent!==null;});
  if (tas.length) {
    var set = Object.getOwnPropertyDescriptor(window.HTMLTextAreaElement.prototype, 'value').set;
    tas.forEach(function(t){
      set.call(t, code);
      t.dispatchEvent(new Event('input', {bubbles: true}));
      t.dispatchEvent(new Event('change', {bubbles: true}));
    });
    return 'textarea:' + tas.length;
  }
  return 'no-editor';
})()`, string(codeBytes))
	var resp string
	if err := safeRun(ctx, chromedp.Evaluate(js, &resp)); err != nil {
		return err
	}
	if resp == "no-editor" {
		return fmt.Errorf("页面上没有找到可写入的编辑器")
	}
	log.Printf("  代码已写入编辑器（%s，%d 字符）", resp, len(code))
	return nil
}

// progapFillInputs 逐空填入答案（input 用原生 setter + input/change 事件触发提交钩子）
func progapFillInputs(ctx context.Context, answers []string) error {
	ansBytes, _ := json.Marshal(answers)
	js := fmt.Sprintf(`(function(){
  var inputs = Array.from(document.querySelectorAll('input')).filter(function(i){
    return i.offsetParent !== null && i.type !== 'hidden' && i.type !== 'radio' && i.type !== 'checkbox';
  });
  var answers = %s;
  if (inputs.length !== answers.length) return 'count-mismatch:' + inputs.length;
  var set = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value').set;
  for (var i = 0; i < inputs.length; i++) {
    inputs[i].scrollIntoView({block: 'center'});
    set.call(inputs[i], answers[i] || '');
    inputs[i].dispatchEvent(new Event('input', {bubbles: true}));
    inputs[i].dispatchEvent(new Event('change', {bubbles: true}));
  }
  return 'ok';
})()`, string(ansBytes))
	var resp string
	if err := safeRun(ctx, chromedp.Evaluate(js, &resp)); err != nil {
		return err
	}
	if resp != "ok" {
		return fmt.Errorf("填入答案失败: %s", resp)
	}
	log.Printf("  已填入 %d 个空", len(answers))
	return nil
}

// progapSubmit 点击提交（并在需要时点确认），随后等待结果
func progapSubmit(ctx context.Context) error {
	// 提交按钮：优先按文本找，涵盖 button / input / a.btn
	var clicked bool
	for _, sel := range []string{
		`//button[contains(., "提交")]`,
		`//a[contains(., "提交")]`,
		`//input[@type="submit"]`,
		`//button[contains(., "保存")]`,
	} {
		clickCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := safeRun(clickCtx, chromedp.Click(sel, chromedp.BySearch, chromedp.NodeVisible))
		cancel()
		if err == nil {
			clicked = true
			log.Printf("  已点击提交按钮（%s）", sel)
			break
		}
	}
	if !clicked {
		return fmt.Errorf("页面上没有找到提交按钮")
	}

	// 弹出确认框：点"确定"
	time.Sleep(1500 * time.Millisecond)
	for _, sel := range []string{
		`//button[contains(., "确定")]`,
		`//button[contains(., "确认")]`,
		`//a[contains(., "确定")]`,
	} {
		okCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
		err := safeRun(okCtx, chromedp.Click(sel, chromedp.BySearch, chromedp.NodeVisible))
		cancel()
		if err == nil {
			log.Printf("  已点击确认按钮（%s）", sel)
			break
		}
	}
	time.Sleep(3 * time.Second)
	return nil
}

// ===================== AI 调用 =====================

// aiProgapCode 整段程序填空题：返回补全后的完整代码
func aiProgapCode(body string) (string, error) {
	sys := `你是C语言程序填空题的答题机器。用户会给出题目正文（含题目描述和待填空的代码片段，空白处用 ____ 或连续下划线标记）。
要求：
1. 只输出补全后的完整C语言代码，保持原有代码结构与缩进不变，仅把空白处填上。
2. 严禁输出 Markdown 代码围栏、解释、题号或任何多余文字。
3. 代码必须能直接编译通过（包含必要头文件由题目决定，不要擅自增删结构）。`
	msgs := []*schema.Message{
		schema.SystemMessage(sys),
		{Role: schema.User, Content: truncate(body, 6000)},
	}
	res, err := ai.ChatModel.Generate(context.Background(), msgs)
	if err != nil {
		return "", fmt.Errorf("AI 生成代码失败: %w", err)
	}
	if res == nil || len(res.Content) == 0 {
		return "", fmt.Errorf("AI 返回内容为空")
	}
	code := ai.CleanCode(res.Content)
	if code == "" {
		return "", fmt.Errorf("AI 返回代码为空")
	}
	return code, nil
}

// aiProgapBlanks 逐空填空：返回 n 行，第 i 行是第 i 个空的答案
func aiProgapBlanks(body string, n int) ([]string, error) {
	sys := fmt.Sprintf(`你是C语言程序填空题的答题机器。题目正文里有 %d 个待填的空（对应 %d 个输入框）。
请输出恰好 %d 行：第 i 行是第 i 个空要填的内容（只填该空本身的代码/表达式/数值，不要整段代码）。
严禁输出编号、解释、引号或多余内容。`, n, n, n)
	msgs := []*schema.Message{
		schema.SystemMessage(sys),
		{Role: schema.User, Content: truncate(body, 6000)},
	}
	res, err := ai.ChatModel.Generate(context.Background(), msgs)
	if err != nil {
		return nil, fmt.Errorf("AI 生成填空失败: %w", err)
	}
	if res == nil || len(res.Content) == 0 {
		return nil, fmt.Errorf("AI 返回内容为空")
	}
	var lines []string
	for _, ln := range strings.Split(ai.CleanCode(res.Content), "\n") {
		ln = strings.TrimSpace(ln)
		if ln != "" {
			lines = append(lines, ln)
		}
	}
	if len(lines) != n {
		return nil, fmt.Errorf("AI 输出 %d 行，期望 %d 行: %q", len(lines), n, lines)
	}
	return lines, nil
}
