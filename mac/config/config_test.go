package config

import (
	"strings"
	"testing"
)

func TestShouldReanswer(t *testing.T) {
	cases := []struct {
		name      string
		threshold float64
		maxTry    int
		score     float64
		try       int
		want      bool
	}{
		// 默认策略：阈值 0、最多 2 次。只有"一分未得"才重答
		{"得0分且还有次数_重答", 0, 2, 0, 1, true},
		{"得分达标_不重答", 0, 2, 2.5, 1, false},
		{"次数用尽_不重答", 0, 2, 0, 2, false},

		// 阈值 -1 表示彻底关闭重答，等价于改造前的行为
		{"阈值为负_永不重答", -1, 3, 0, 1, false},
		{"阈值为负_得了负分也不重答", -1, 3, -5, 1, false},

		// 更激进的策略：低于 2.5 分就重答
		{"激进阈值_低分重答", 2.5, 3, 2, 1, true},
		{"激进阈值_刚好等于阈值也重答", 2.5, 3, 2.5, 2, true},
		{"激进阈值_超阈值不重答", 2.5, 3, 3, 1, false},

		// 边界：刚好到次数上限
		{"首次即用尽次数_不重答", 0, 1, 0, 1, false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := Config{ReanswerThreshold: c.threshold, MaxAnswerTry: c.maxTry}
			if got := cfg.ShouldReanswer(c.score, c.try); got != c.want {
				t.Errorf("ShouldReanswer(score=%v, try=%d) = %v, 期望 %v",
					c.score, c.try, got, c.want)
			}
		})
	}
}

func TestEnvHelpersFallbackToDefault(t *testing.T) {
	// 未设置时取默认值
	t.Setenv("TEST_KEY_NOT_SET", "")
	if got := envOr("TEST_KEY_NOT_SET", "fallback"); got != "fallback" {
		t.Errorf("envOr 空值应回退默认值，得到 %q", got)
	}
	// 只有空白也算未设置
	t.Setenv("TEST_KEY_BLANK", "   ")
	if got := envOr("TEST_KEY_BLANK", "fallback"); got != "fallback" {
		t.Errorf("envOr 全空白应回退默认值，得到 %q", got)
	}
	// 有值时取环境变量
	t.Setenv("TEST_KEY_SET", "value")
	if got := envOr("TEST_KEY_SET", "fallback"); got != "value" {
		t.Errorf("envOr 应取环境变量，得到 %q", got)
	}
}

func TestEnvIntAndFloat(t *testing.T) {
	t.Setenv("TEST_INT_OK", "7")
	if got := envInt("TEST_INT_OK", 1); got != 7 {
		t.Errorf("envInt 应解析为 7，得到 %d", got)
	}
	t.Setenv("TEST_INT_BAD", "abc")
	if got := envInt("TEST_INT_BAD", 3); got != 3 {
		t.Errorf("envInt 解析失败应回退默认值 3，得到 %d", got)
	}
	t.Setenv("TEST_INT_EMPTY", "")
	if got := envInt("TEST_INT_EMPTY", 5); got != 5 {
		t.Errorf("envInt 未设置应回退默认值 5，得到 %d", got)
	}

	t.Setenv("TEST_FLOAT_OK", "2.5")
	if got := envFloat("TEST_FLOAT_OK", 0); got != 2.5 {
		t.Errorf("envFloat 应解析为 2.5，得到 %v", got)
	}
	t.Setenv("TEST_FLOAT_BAD", "x")
	if got := envFloat("TEST_FLOAT_BAD", -1); got != -1 {
		t.Errorf("envFloat 解析失败应回退默认值 -1，得到 %v", got)
	}
}

func TestLoadAppliesDefaults(t *testing.T) {
	// 清掉可能干扰的环境变量
	for _, k := range []string{
		"CDP_PORT", "CHROME_USER_DATA", "ASSIGN_KEYWORD",
		"REANSWER_THRESHOLD", "MAX_ANSWER_TRY", "PROMPTS_FILE", "LOG_FORMAT",
	} {
		t.Setenv(k, "")
	}

	cfg := Load()

	if cfg.CDPPort != DefaultCDPPort {
		t.Errorf("CDPPort 默认应为 %q，得到 %q", DefaultCDPPort, cfg.CDPPort)
	}
	if cfg.ChromeUserData != DefaultChromeProfile {
		t.Errorf("ChromeUserData 默认应为 %q，得到 %q", DefaultChromeProfile, cfg.ChromeUserData)
	}
	if cfg.AssignKeyword != DefaultAssignKeyword {
		t.Errorf("AssignKeyword 默认应为 %q，得到 %q", DefaultAssignKeyword, cfg.AssignKeyword)
	}
	if cfg.ReanswerThreshold != DefaultReanswerThreshold {
		t.Errorf("ReanswerThreshold 默认应为 %v，得到 %v", DefaultReanswerThreshold, cfg.ReanswerThreshold)
	}
	if cfg.MaxAnswerTry != DefaultMaxAnswerTry {
		t.Errorf("MaxAnswerTry 默认应为 %d，得到 %d", DefaultMaxAnswerTry, cfg.MaxAnswerTry)
	}
	if cfg.IsJSONLog() {
		t.Error("默认日志格式不应是 JSON")
	}
}

func TestLoadClampsMaxAnswerTry(t *testing.T) {
	// MaxAnswerTry 至少为 1，否则流程一步都走不下去
	t.Setenv("MAX_ANSWER_TRY", "0")
	if got := Load().MaxAnswerTry; got != 1 {
		t.Errorf("MAX_ANSWER_TRY=0 应被夹到 1，得到 %d", got)
	}
	t.Setenv("MAX_ANSWER_TRY", "-3")
	if got := Load().MaxAnswerTry; got != 1 {
		t.Errorf("MAX_ANSWER_TRY=-3 应被夹到 1，得到 %d", got)
	}
}

func TestLoadReadsJSONLogFlag(t *testing.T) {
	t.Setenv("LOG_FORMAT", "JSON")
	if !Load().IsJSONLog() {
		t.Error("LOG_FORMAT=JSON 应被识别（大小写不敏感）")
	}
}

func TestUserDataDirFallsBackToDefault(t *testing.T) {
	C = Config{ChromeUserData: ""}
	if got := UserDataDir(); got != DefaultChromeProfile {
		t.Errorf("未配置时应回退到 %q，得到 %q", DefaultChromeProfile, got)
	}
	C = Config{ChromeUserData: "/tmp/custom-profile"}
	if got := UserDataDir(); got != "/tmp/custom-profile" {
		t.Errorf("已配置时应原样返回，得到 %q", got)
	}
}

func TestFindChromeHonoursEnvOverride(t *testing.T) {
	C = Config{ChromePath: "/nonexistent/but/specified/chrome"}
	// 显式指定时不校验存在性，直接返回（让调用方去报错）
	got, err := FindChrome()
	if err != nil {
		t.Fatalf("显式指定 CHROME_PATH 时不应报错: %v", err)
	}
	if got != "/nonexistent/but/specified/chrome" {
		t.Errorf("应原样返回指定路径，得到 %q", got)
	}
}

func TestPythonCandidatesHonoursEnvOverride(t *testing.T) {
	C = Config{PythonBin: "/custom/python"}
	got := PythonCandidates()
	if len(got) != 1 || got[0] != "/custom/python" {
		t.Errorf("指定 PYTHON_BIN 后应只返回该路径，得到 %v", got)
	}
}

func TestSelectorsAreNonEmpty(t *testing.T) {
	// 选择器是页面的硬契约，空值会导致定位静默失败，这里做一道兜底防线
	selectors := map[string]string{
		"SelUsername":          SelUsername,
		"SelPassword":          SelPassword,
		"SelCaptchaCode":       SelCaptchaCode,
		"SelCaptchaImg":        SelCaptchaImg,
		"SelLoginBtn":          SelLoginBtn,
		"SelQuizForm":          SelQuizForm,
		"SelQuizInput":         SelQuizInput,
		"SelProgapCodeArea":    SelProgapCodeArea,
		"SelProgapSubmitBtn":   SelProgapSubmitBtn,
		"SelProgapAnswer":      SelProgapAnswer,
		"SelProgapResultFrame": SelProgapResultFrame,
		"URLLogin":             URLLogin,
	}
	for name, v := range selectors {
		if v == "" {
			t.Errorf("%s 不应为空", name)
		}
	}
}

func TestLoginURLCarriesErrParam(t *testing.T) {
	// 登录页 URL 必须带 loginErr=0，否则站点行为不同
	if !strings.Contains(URLLogin, "loginErr=0") {
		t.Errorf("URLLogin 应携带 loginErr=0，得到 %q", URLLogin)
	}
	if !strings.Contains(URLLogin, "simple.jsp") {
		t.Errorf("URLLogin 应指向 simple.jsp，得到 %q", URLLogin)
	}
	if !strings.HasPrefix(URLLogin, URLSiteRoot) {
		t.Errorf("URLLogin 应以站点根地址开头，得到 %q", URLLogin)
	}
}
