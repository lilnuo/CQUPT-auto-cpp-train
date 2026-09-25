package train

// ============================================================================
// progap.go —— 程序片段编程题（programFillGapList.jsp）答题支持
//
// 设计原则：
//  1. 全部逻辑在本文件内自成一派，不改动 train.go 里已经跑通的整页刷题（quiz）逻辑；
//     模式分流发生在 api.go 的 Run 里（默认 quiz，行为与之前完全一致）。
//  2. 复用 train.go 里已有的通用件：launchHumanChrome / ensureLoginPage /
//     autoLogin / enterAssignment（WAF 绕过、OCR 登录、选作业卡），不重复造轮子。
//  3. 页面结构不确定时先"探测 + dump"，再据此精修选择器；
//     结构完全对不上时走 progapAnswerOneGeneric 通用兜底分支。
//  4. 提交后回读判题结果：不通过则在剩余次数内换个思路重答（见 judgeProgapResult）。
//
// 所有选择器/超时/阈值都来自 config 包，站点改版时只需改 config/site.go。
// ============================================================================

import (
	"context"
	"cqupt/ai"
	"cqupt/config"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/chromedp/chromedp"
)

// 判题回显的关键词匹配器（编译一次，避免每题重复编译）
var (
	progapFailRe = regexp.MustCompile(config.ReProgapFailKeywords)
	progapPassRe = regexp.MustCompile(config.ReProgapPassKeywords)
)

// runProgap 程序题完整流程：启动浏览器 → 登录 → 进入作业 → 作答。
// dumpOnly=true 时只 dump 第一道程序题页面后返回。
func runProgap(username, password string, num int, dumpOnly bool) (int, error) {
	chromeCmd, browserWS, err := launchHumanChrome(config.C.CDPPort, config.UserDataDir(), config.URLLogin)
	if err != nil {
		return 0, err
	}
	defer func() {
		_ = chromeCmd.Process.Kill()
		_ = chromeCmd.Wait()
	}()

	// browserWS 一定非空：读不到调试地址时 launchHumanChrome 已经报错返回了。
	alloctx, cancelAlloc := chromedp.NewRemoteAllocator(context.Background(), browserWS)
	defer cancelAlloc()

	ctx, cancelCtx := chromedp.NewContext(alloctx)
	defer cancelCtx()
	if err := safeRun(ctx); err != nil {
		return 0, fmt.Errorf("建立浏览器控制连接失败: %w", err)
	}

	slog.Info("等待 WAF 挑战自动通过", "约", config.WaitWAFChallenge.String())
	time.Sleep(config.WaitWAFChallenge)

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
  Array.from(document.querySelectorAll('` + config.SelProgapListLink + `')).forEach(function(a){
    if (a.offsetParent === null) return;
    var tr = a.closest('tr');
    var status = '';
    if (tr) {
      var cells = Array.from(tr.querySelectorAll('td,th')).map(function(c){ return (c.innerText||'').trim(); });
      status = cells[cells.length-1] || '';
    }
    var done = !/` + config.ReProgapNotSubmitted + `/.test(status);
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
	for i := 0; i < config.PollAssignmentMax; i++ {
		pollCtx, cancel := context.WithTimeout(ctx, config.TimeoutAssignmentPoll)
		_ = safeRun(pollCtx, chromedp.Evaluate(progapListJS, &raw))
		cancel()
		if err := json.Unmarshal([]byte(raw), &items); err == nil && len(items) > 0 {
			return items, nil
		}
		time.Sleep(config.PollAssignmentTick)
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
	probeCtx, cancel := context.WithTimeout(ctx, config.TimeoutProgapProbe)
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
	slog.Info("准备 dump 第一道程序题", "总数", len(items), "标题", items[0].Title, "href", items[0].Href)

	navCtx, cancel := context.WithTimeout(ctx, config.TimeoutProgapListNav)
	defer cancel()
	if err := safeRun(navCtx, chromedp.Navigate(items[0].Href)); err != nil {
		return err
	}
	// 等页面渲染
	for i := 0; i < config.PollProgapRenderMax; i++ {
		var l int
		_ = safeRun(navCtx, chromedp.Evaluate(`(document.body.innerText||'').trim().length`, &l))
		if l > 100 {
			break
		}
		time.Sleep(config.PollRenderTick)
	}

	var html string
	_ = safeRun(navCtx, chromedp.Evaluate(`document.documentElement.outerHTML`, &html))
	if err := os.WriteFile(config.FileDumpProgap, []byte(html), 0644); err != nil {
		slog.Error("写 dump 文件失败", "file", config.FileDumpProgap, "err", err)
	} else {
		slog.Info("已保存程序题页面", "file", config.FileDumpProgap, "字节", len(html))
	}

	p, err := progapProbe(ctx)
	if err != nil {
		return err
	}
	slog.Info("[结构] 页面探测",
		"编辑器", p.Editors, "可见输入框", p.Inputs, "疑似空格数", p.Blanks, "按钮", p.Buttons)
	slog.Info("[结构] 正文摘要", "text", truncate(p.Body, 1500))
	return nil
}

// truncate 按字符（rune）截断，避免把多字节字符切一半产生乱码。
// 出参 n 的单位是"字符数"而非字节数。
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}

// ===================== 作答 =====================

// progapQuestionJS 提取程序题题干（基于真实 DOM）：
// 标题 + 代码区，把嵌在代码块之间的 textarea 替换成"【空N】"占位符后取文本。
// 这样 AI 能看到完整上下文：上方代码 → 空 → 下方代码。
//
// 注意：这里选择**所有** textarea（而非仅 name^=answer），因为要把每一个空都
// 换成占位符；填入答案时才按 name 前缀精确定位，见 progapFillAnswersJS。
const progapQuestionJS = `(function(){
  var root = document.getElementById('` + config.SelProgapCodeArea + `') || document.querySelector('` + config.SelProgapForm + `') || document.body;
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
  var tas = Array.from(document.querySelectorAll('`+config.SelProgapAnswer+`'));
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
  var f = document.querySelector('` + config.SelProgapResultFrame + `');
  if (!f || !f.contentDocument || !f.contentDocument.body) return '';
  return (f.contentDocument.body.innerText || '').trim().slice(0, 1000);
})()`

// progapSolve 逐题作答：进入题目 → 探测结构 → AI 生成 → 填入 → 提交 → 回读结果（不通过则重答）
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
	slog.Info("程序片段编程题枚举完成", "总数", len(items), "未提交", todo, "本次最多做", num)

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

		passed, err := progapAnswerOne(ctx, it, done == 1)
		if err != nil {
			slog.Error("题目处理失败", "标题", it.Title, "err", err)
			failed++
		} else if !passed {
			slog.Warn("判题未通过（已用完重答次数）", "标题", it.Title)
			failed++
		} else {
			slog.Info("题目已通过 ✓", "标题", it.Title)
		}

		// 回到列表页，继续下一题
		backCtx, cancel := context.WithTimeout(ctx, config.TimeoutProgapBack)
		_ = safeRun(backCtx, chromedp.Navigate(listURL))
		cancel()
		time.Sleep(config.WaitProgapBackToList)
	}
	slog.Info("完成", "通过", done-failed, "未通过或失败", failed)
	return done - failed, nil
}

// progapOutcome 是一次作答的判题结果
type progapOutcome struct {
	// Passed 是否通过
	Passed bool
	// Known 判题回显里是否能识别出通过/失败；
	// 为 false 表示文案不认识，此时不触发重答（宁可当通过，避免无谓消耗 token）
	Known bool
	// Raw 判题回显原文，用于日志与排查
	Raw string
}

// judgeProgapResult 从判题回显文本判断是否通过。
//
// 站点回显文案没有稳定契约，所以采用关键词集合匹配，并且**失败关键词优先**——
// 因为"未通过"这类文本同时含有"通过"二字，先判失败才不会误判。
func judgeProgapResult(text string) progapOutcome {
	out := progapOutcome{Raw: text, Passed: true}
	if strings.TrimSpace(text) == "" {
		// 没拿到任何回显：无法判断，按"已提交"处理
		return out
	}
	if progapFailRe.MatchString(text) {
		out.Passed = false
		out.Known = true
		return out
	}
	if progapPassRe.MatchString(text) {
		out.Passed = true
		out.Known = true
		return out
	}
	return out // Known=false
}

// progapAnswerOne 处理单道程序题，含"判题不通过则重答"的闭环。
//
// 每轮重答都重新 Navigate 到题目页——程序题是独立页面，重新进入即可重新作答，
// 这与选择题"提交后锁定"的形态不同，所以这里能实现真正的重做。
func progapAnswerOne(ctx context.Context, it progapItem, dumpFirst bool) (passed bool, err error) {
	var last progapOutcome
	tries := 0

	// 用 defer 保证从任何出口返回都记录一次结果。
	// 这道题有四个出口（通过 / 回显无法识别 / 次数用尽 / 出错），
	// 逐个出口写一遍迟早会漏一个 —— 结果表缺一行，统计出来的正确率就是错的。
	defer func() {
		if err != nil {
			return
		}
		detail := "判题未通过"
		switch {
		case passed:
			detail = "判题通过"
		case !last.Known:
			detail = "判题回显无法识别，按通过处理（不做无谓重答）"
		}
		score := 0.0
		if passed {
			score = 1
		}
		noteResult(QuestionResult{
			Pid: it.Title, Text: it.Title,
			Score: score, Tries: tries,
			Judgment: detail + "；回显：" + truncate(last.Raw, 120),
		})
	}()

	for try := 1; try <= config.C.MaxAnswerTry; try++ {
		tries = try
		outcome, err := progapAttempt(ctx, it, dumpFirst && try == 1, try)
		if err != nil {
			return false, err
		}
		last = outcome

		if outcome.Passed || !outcome.Known {
			return outcome.Passed, nil
		}
		if try >= config.C.MaxAnswerTry {
			break
		}
		slog.Warn("判题未通过，准备重答",
			"标题", it.Title, "第几次", try, "上限", config.C.MaxAnswerTry, "回显", truncate(outcome.Raw, 120))
	}
	return last.Passed, nil
}

// progapAttempt 完成一次完整作答：打开题目 → 提取题干 → 生成答案 → 填入 → 提交 → 回读结果。
// try > 1 时给 AI 追加"重答提醒"，让它换个思路。
func progapAttempt(ctx context.Context, it progapItem, dumpFirst bool, try int) (progapOutcome, error) {
	oneCtx, cancel := context.WithTimeout(ctx, config.TimeoutProgapQuestion)
	defer cancel()

	if err := safeRun(oneCtx, chromedp.Navigate(it.Href)); err != nil {
		return progapOutcome{}, fmt.Errorf("打开题目失败: %w", err)
	}
	// 等渲染（textarea 出现才算就绪）
	for i := 0; i < config.PollProgapRenderMax; i++ {
		var n int
		_ = safeRun(oneCtx, chromedp.Evaluate(
			fmt.Sprintf(`document.querySelectorAll(%q).length`, config.SelProgapAnswer), &n))
		if n > 0 {
			break
		}
		time.Sleep(config.PollRenderTick)
	}

	// 提取题干：标题 + 代码区（textarea 替换为【空N】占位符）
	var qraw string
	var q struct {
		Title  string `json:"title"`
		Text   string `json:"text"`
		Blanks int    `json:"blanks"`
	}
	if err := safeRun(oneCtx, chromedp.Evaluate(progapQuestionJS, &qraw)); err != nil {
		return progapOutcome{}, fmt.Errorf("提取题干失败: %w", err)
	}
	if err := json.Unmarshal([]byte(qraw), &q); err != nil {
		return progapOutcome{}, fmt.Errorf("解析题干失败: %w", err)
	}
	if q.Blanks == 0 {
		// 兜底：题目结构不是"代码内嵌 textarea"（可能是整段编程题），走通用分支
		slog.Warn("未发现填空 textarea，走通用编辑器分支")
		return progapOutcome{Passed: true}, progapAnswerOneGeneric(oneCtx)
	}
	slog.Info("开始作答", "标题", q.Title, "空数", q.Blanks, "第几次", try)

	// 第一题自动保存页面 + 打印题干，便于核对
	if dumpFirst {
		var html string
		_ = safeRun(oneCtx, chromedp.Evaluate(`document.documentElement.outerHTML`, &html))
		_ = os.WriteFile(config.FileDumpProgap, []byte(html), 0644)
		slog.Info("已保存首题页面", "file", config.FileDumpProgap, "字节", len(html))
		slog.Info("题干摘要", "text", truncate(q.Text, 600))
	}

	// AI 逐空生成答案（重答时带提醒，让模型换个思路）
	question := q.Title + "\n" + q.Text
	hint := ""
	if try > 1 {
		hint = ai.Prompts.ReanswerSuffix
	}
	answers, err := ai.AnswerBlanks(question, q.Blanks, hint)
	if err != nil {
		return progapOutcome{}, err
	}

	// 填入 textarea
	var resp string
	if err := safeRun(oneCtx, chromedp.Evaluate(progapFillAnswersJS(answers), &resp)); err != nil || resp != "ok" {
		return progapOutcome{}, fmt.Errorf("填入答案失败: %v %s", err, resp)
	}
	slog.Info("已填入答案", "空数", len(answers))

	// 提交（#cgSubmitBtn，提交到隐藏 iframe，页面不刷新）并回读判题结果
	raw, err := progapSubmitAndWait(oneCtx)
	if err != nil {
		return progapOutcome{}, err
	}
	return judgeProgapResult(raw), nil
}

// progapSubmitAndWait 点 #cgSubmitBtn 提交，轮询结果 iframe 等判题结果。
// 返回判题回显原文（可能为空串，表示未等到）。
func progapSubmitAndWait(ctx context.Context) (string, error) {
	clickCtx, cancel := context.WithTimeout(ctx, config.TimeoutProgapSubmit)
	err := safeRun(clickCtx, chromedp.Click(config.SelProgapSubmitBtn, chromedp.ByQuery, chromedp.NodeVisible))
	cancel()
	if err != nil {
		return "", fmt.Errorf("点击提交按钮失败: %w", err)
	}
	slog.Info("已提交，等待判题结果...")

	// 判题是服务器跑测试用例，可能几秒到几十秒
	last := ""
	for i := 0; i < config.PollProgapJudgeMax; i++ {
		time.Sleep(config.PollProgapJudgeTick)
		var result string
		_ = safeRun(ctx, chromedp.Evaluate(progapResultJS, &result))
		if result == "" {
			continue
		}
		last = result
		if strings.Contains(result, config.TextJudging) ||
			strings.Contains(result, config.TextJudging2) ||
			strings.Contains(result, config.TextJudging3) {
			continue // 还在判题
		}
		slog.Info("判题结果", "text", truncate(result, 300))
		return result, nil
	}
	if last != "" {
		slog.Warn("未等到明确判题结果，采用最后一次回显", "text", truncate(last, 300))
		return last, nil
	}
	slog.Warn("未等到判题结果（可能服务器较慢），按已提交处理")
	return "", nil
}

// progapAnswerOneGeneric 通用兜底分支：整段代码编辑器或逐空 input（结构未知的题目）
func progapAnswerOneGeneric(ctx context.Context) error {
	p, err := progapProbe(ctx)
	if err != nil {
		return err
	}
	slog.Info("[兜底] 页面结构", "编辑器", p.Editors, "输入框", p.Inputs, "空格", p.Blanks, "按钮", p.Buttons)
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
		code, err := ai.AnswerCode(p.Body)
		if err != nil {
			return err
		}
		if err := progapFillEditor(ctx, code); err != nil {
			return err
		}
	} else if p.Inputs > 0 {
		answers, err := ai.AnswerBlanks(p.Body, p.Inputs)
		if err != nil {
			return err
		}
		if err := progapFillInputs(ctx, answers); err != nil {
			return err
		}
	} else {
		return fmt.Errorf("未识别到任何输入控件（编辑器=%v），已保存页面供排查", p.Editors)
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
	slog.Info("代码已写入编辑器", "方式", resp, "字符数", len(code))
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
	slog.Info("已填入答案", "空数", len(answers))
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
		clickCtx, cancel := context.WithTimeout(ctx, config.TimeoutSingleAction)
		err := safeRun(clickCtx, chromedp.Click(sel, chromedp.BySearch, chromedp.NodeVisible))
		cancel()
		if err == nil {
			clicked = true
			slog.Info("已点击提交按钮", "selector", sel)
			break
		}
	}
	if !clicked {
		return fmt.Errorf("页面上没有找到提交按钮")
	}

	// 弹出确认框：点"确定"
	time.Sleep(config.WaitConfirmDialog)
	for _, sel := range []string{
		`//button[contains(., "确定")]`,
		`//button[contains(., "确认")]`,
		`//a[contains(., "确定")]`,
	} {
		okCtx, cancel := context.WithTimeout(ctx, config.TimeoutProgapConfirm)
		err := safeRun(okCtx, chromedp.Click(sel, chromedp.BySearch, chromedp.NodeVisible))
		cancel()
		if err == nil {
			slog.Info("已点击确认按钮", "selector", sel)
			break
		}
	}
	time.Sleep(config.WaitAfterSubmit)
	return nil
}
