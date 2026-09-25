package train

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"cqupt/config"
)

// ============================================================================
// JS 拼装：答案必须经 JSON 转义后嵌入
//
// 这是本项目最隐蔽的一类 bug 来源——答案里只要出现引号或换行，
// 直接字符串拼接就会生成语法错误的 JS，表现为"填不进去"而没有任何报错。
// ============================================================================

func TestQuizFillJS_MarshalsAnswersSafely(t *testing.T) {
	// 覆盖三类高危字符：双引号、换行、反斜杠
	answers := []string{`he said "hi"`, "第一行\n第二行", `反斜杠\测试`}
	js := quizFillJS("123", answers)

	wantJSON, err := json.Marshal(answers)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(js, string(wantJSON)) {
		t.Errorf("答案应以 JSON 字面量嵌入，期望包含 %s\n实际 JS:\n%s", wantJSON, js)
	}

	// 表单名与空数应出现在 JS 里
	if !strings.Contains(js, "answerForm123") {
		t.Error("JS 应引用 answerForm123")
	}
	if !strings.Contains(js, "inputs.length !== 3") {
		t.Errorf("JS 应校验空数为 3，实际:\n%s", js)
	}
}

func TestQuizFillJS_SingleAnswer(t *testing.T) {
	js := quizFillJS("7", []string{"A"})
	if !strings.Contains(js, "answerForm7") {
		t.Error("JS 应引用 answerForm7")
	}
	if !strings.Contains(js, "inputs.length !== 1") {
		t.Error("JS 应校验空数为 1")
	}
}

func TestQuizFillJS_UsesSharedSelector(t *testing.T) {
	// 选择器来自 config 常量拼接，改 config 即可全项目生效
	js := quizFillJS("1", []string{"A"})
	if !strings.Contains(js, config.SelQuizInput) {
		t.Errorf("JS 应包含集中定义的输入框选择器 %q", config.SelQuizInput)
	}
}

func TestProgapFillAnswersJS_MarshalsAnswersSafely(t *testing.T) {
	answers := []string{"printf(\"hello\\n\");", "i++", `if (a == "x") {}`}
	js := progapFillAnswersJS(answers)

	wantJSON, _ := json.Marshal(answers)
	if !strings.Contains(js, string(wantJSON)) {
		t.Errorf("答案应以 JSON 字面量嵌入，期望包含 %s\n实际 JS:\n%s", wantJSON, js)
	}
	if !strings.Contains(js, config.SelProgapAnswer) {
		t.Errorf("JS 应包含集中定义的 textarea 选择器 %q", config.SelProgapAnswer)
	}
	if !strings.Contains(js, "tas.length !== answers.length") {
		t.Error("JS 应校验填入数量与 textarea 数量一致")
	}
}

func TestJSConstantsCarrySharedSelectors(t *testing.T) {
	// 这些 JS 里的选择器全部来自 config，一旦有人写回硬编码，这里会失败
	cases := []struct {
		name string
		js   string
		want string
	}{
		{"quizListJS 的题目表单", quizListJS, config.SelQuizForm},
		{"quizListJS 的状态提示前缀", quizListJS, config.SelQuizSaveTipPrefix},
		{"quizListJS 的已提交文案", quizListJS, config.TextQuizSubmitted},
		{"quizScoreJS 的总分正则", quizScoreJS, config.ReTotalScoreJS},
		{"progapListJS 的题目链接", progapListJS, config.SelProgapListLink},
		{"progapQuestionJS 的代码区", progapQuestionJS, config.SelProgapCodeArea},
		{"progapResultJS 的结果 iframe", progapResultJS, config.SelProgapResultFrame},
		{"assignmentCardsJS 的进入作业文案", assignmentCardsJS, config.ReAssignmentBtn},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if !strings.Contains(c.js, c.want) {
				t.Errorf("该 JS 应包含 %q", c.want)
			}
		})
	}
}

func TestQuizTipJS_EscapesPidAsStringLiteral(t *testing.T) {
	// pid 来自页面，理论上可控，但仍按字符串字面量转义，不做裸拼接
	js := quizTipJS(`12"34`)
	if strings.Contains(js, `'saveTip12"34'`) {
		t.Error("pid 应转义后再嵌入，不应出现裸引号把 JS 字符串截断")
	}
	if !strings.Contains(js, config.SelQuizSaveTipPrefix) {
		t.Error("应使用集中定义的状态提示前缀")
	}
}

// ============================================================================
// 判题结果判定
// ============================================================================

func TestJudgeProgapResult(t *testing.T) {
	cases := []struct {
		name       string
		text       string
		wantPassed bool
		wantKnown  bool
	}{
		// 失败关键词优先，否则"未通过"会被"通过"二字误判为成功
		{"未通过不能被误判", "未通过，请检查输出格式", false, true},
		{"不通过", "答案不通过", false, true},
		{"编译错误", "编译错误：第 3 行缺少分号", false, true},
		{"答案错误", "答案错误，得分 0", false, true},
		{"英文错误回显（大小写不敏感）", "WRONG ANSWER", false, true},
		{"英文小写", "error: expected ';'", false, true},

		{"明确通过", "恭喜，答案正确！", true, true},
		{"中文通过", "检查通过", true, true},
		{"英文通过", "Accepted", true, true},

		// 拿不准的一律按通过处理，避免无谓地重刷消耗 token
		{"空回显 无法判定", "", true, false},
		{"纯空白 无法判定", "   ", true, false},
		{"不认识的中性文案", "提交成功，已结束", true, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := judgeProgapResult(c.text)
			if got.Passed != c.wantPassed {
				t.Errorf("judgeProgapResult(%q).Passed = %v，期望 %v", c.text, got.Passed, c.wantPassed)
			}
			if got.Known != c.wantKnown {
				t.Errorf("judgeProgapResult(%q).Known = %v，期望 %v", c.text, got.Known, c.wantKnown)
			}
			if got.Raw != c.text {
				t.Errorf("Raw 应原样保存回显文本，得到 %q", got.Raw)
			}
		})
	}
}

// ============================================================================
// truncate：按字符截断，不能把中文切出乱码
// ============================================================================

func TestTruncate(t *testing.T) {
	cases := []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"短串原样返回", "你好", 10, "你好"},
		{"恰好等于上限", "你好世界", 4, "你好世界"},
		{"超长按字符截断", "你好世界再见", 4, "你好世界..."},
		{"上限为 0", "abc", 0, "..."},
		{"单个中文字符上限", "你好世界", 1, "你..."},
		{"空串", "", 5, ""},
		{"纯 ASCII 截断", "abcdefg", 3, "abc..."},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := truncate(c.in, c.n)
			if got != c.want {
				t.Errorf("truncate(%q, %d) = %q，期望 %q", c.in, c.n, got, c.want)
			}
			// 关键：截断后不能出现半个多字节字符
			if !utf8.ValidString(got) {
				t.Errorf("truncate(%q, %d) 产生了非法 UTF-8: %q", c.in, c.n, got)
			}
		})
	}
}

func TestTruncateNeverSplitsRune(t *testing.T) {
	// 用各种长度的中文串暴力验证：任何切点都不能产生乱码
	s := "空山新雨后天气晚来秋明月松间照清泉石上流"
	for n := 0; n <= len(s)+2; n++ {
		got := truncate(s, n)
		if !utf8.ValidString(got) {
			t.Fatalf("在 n=%d 处切出了非法 UTF-8: %q", n, got)
		}
		if !strings.HasSuffix(got, "...") && n < utf8.RuneCountInString(s) {
			t.Errorf("n=%d 时未截断却也没加省略号: %q", n, got)
		}
	}
}

// ============================================================================
// quizItem / progapItem 的 JSON 标签要与页面回传的字段名一致
// ============================================================================

func TestQuizItemUnmarshalsFromJSOutput(t *testing.T) {
	// 模拟 quizListJS 的返回结构
	raw := `[{"pid":"123","text":"第1题","n":2,"answered":false}]`
	var items []quizItem
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		t.Fatalf("应能解析 quizListJS 的输出: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("应解析出 1 项，得到 %d", len(items))
	}
	it := items[0]
	if it.Pid != "123" || it.Text != "第1题" || it.N != 2 || it.Answered {
		t.Errorf("解析结果不符合预期: %+v", it)
	}
}

func TestProgapItemUnmarshalsFromJSOutput(t *testing.T) {
	raw := `[{"i":0,"title":"题目A","href":"/x.jsp","status":"还未提交","done":false}]`
	var items []progapItem
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		t.Fatalf("应能解析 progapListJS 的输出: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("应解析出 1 项，得到 %d", len(items))
	}
	it := items[0]
	if it.I != 0 || it.Title != "题目A" || it.Href != "/x.jsp" || it.Done {
		t.Errorf("解析结果不符合预期: %+v", it)
	}
}

// ============================================================================
// 探测结构的解析
// ============================================================================

func TestProgapProbeInfoUnmarshal(t *testing.T) {
	raw := `{"editors":["textarea:2"],"inputs":0,"buttons":["提 交"],"body":"代码","blanks":2}`
	var p progapProbeInfo
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		t.Fatalf("应能解析探测结果: %v", err)
	}
	if len(p.Editors) != 1 || p.Editors[0] != "textarea:2" {
		t.Errorf("editors 解析异常: %v", p.Editors)
	}
	if p.Blanks != 2 || p.Body != "代码" {
		t.Errorf("探测字段解析异常: %+v", p)
	}
}

// ============================================================================
// 分页/拼装相关的辅助函数
// ============================================================================

func TestAssignmentClickJS_EmbedsIndex(t *testing.T) {
	js := assignmentClickJS(3)
	if !strings.Contains(js, "btns[3]") {
		t.Errorf("应点击第 3 个按钮，实际 JS:\n%s", js)
	}
	if !strings.Contains(js, config.ReAssignmentBtn) {
		t.Error("应使用集中定义的按钮文案正则")
	}
}

func TestFindChromeErrorIsActionable(t *testing.T) {
	// CHROME_PATH 指向不存在的路径时，应把路径原样交出（由 Chrome 启动阶段报错），
	// 而不是在这里做存在性校验——用户可能用的是一个符号链接或 wrapper 脚本。
	old := config.C
	defer func() { config.C = old }()

	config.C.ChromePath = "/definitely/not/here/chrome"
	got, err := config.FindChrome()
	if err != nil {
		t.Fatalf("显式指定路径时不应报错: %v", err)
	}
	if got != "/definitely/not/here/chrome" {
		t.Errorf("应原样返回，得到 %q", got)
	}
}
