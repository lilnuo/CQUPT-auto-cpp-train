package ai

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ============================================================================
// CleanCode：剥掉 Markdown 代码围栏
// 这是全项目最容易出错也最值得测的一环——模型十次有九次会加围栏
// ============================================================================

func TestCleanCode(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"无围栏原样返回", "int main(){}", "int main(){}"},
		{"c 围栏", "```c\nint main(){}", "int main(){}"},
		{"cpp 围栏", "```cpp\nint main(){}", "int main(){}"},
		{"无语言围栏", "```\nint main(){}", "int main(){}"},
		{"带 # 的语言标记", "```c#\nint main(){}", "int main(){}"},
		{"完整闭合围栏", "```c\nint main(){\n  return 0;\n}\n```", "int main(){\n  return 0;\n}"},
		{"首尾多余空白", "\n\n   int a;   \n\n", "int a;"},
		{"空串", "", ""},
		{"只有空白", "   \n\t  ", ""},
		{"多行代码保留内部换行", "```c\nint a;\nint b;\n```", "int a;\nint b;"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := CleanCode(c.in); got != c.want {
				t.Errorf("CleanCode(%q) = %q，期望 %q", c.in, got, c.want)
			}
		})
	}
}

// ============================================================================
// SplitNonEmptyLines：把"每空一行"的模型输出切成答案列表
// ============================================================================

func TestSplitNonEmptyLines(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"普通三行", "A\nB\nC", []string{"A", "B", "C"}},
		{"丢掉空行", "A\n\nB\n\n\nC", []string{"A", "B", "C"}},
		{"每行去首尾空白", "  A  \n\tB\t", []string{"A", "B"}},
		{"先剥围栏再切行", "```\nA\nB\n```", []string{"A", "B"}},
		{"整个输出为空", "", nil},
		{"只有空白行", "\n\n\n", nil},
		{"单行", "42", []string{"42"}},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := SplitNonEmptyLines(c.in)
			if len(got) != len(c.want) {
				t.Fatalf("SplitNonEmptyLines(%q) 长度 = %d (%v)，期望 %d (%v)",
					c.in, len(got), got, len(c.want), c.want)
			}
			for i := range got {
				if got[i] != c.want[i] {
					t.Errorf("第 %d 项 = %q，期望 %q", i, got[i], c.want[i])
				}
			}
		})
	}
}

// ============================================================================
// firstUpperLetter：从模型输出里稳健地捞选项字母
// 模型经常多吐句号、引号、甚至"答案是 A"，必须能容错
// ============================================================================

func TestFirstUpperLetter(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{"单个大写字母", "A", "A", true},
		{"小写转大写", "b", "B", true},
		{"带空白", "  c  ", "C", true},
		{"带标点", "D。", "D", true},
		{"带解释前缀", "答案是 B", "B", true},
		{"带引号", `"E"`, "E", true},
		{"没有任何字母", "123", "", false},
		{"空串", "", "", false},
		{"只有标点", "。！？", "", false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := firstUpperLetter(c.in)
			if ok != c.ok {
				t.Fatalf("firstUpperLetter(%q) ok = %v，期望 %v", c.in, ok, c.ok)
			}
			if got != c.want {
				t.Errorf("firstUpperLetter(%q) = %q，期望 %q", c.in, got, c.want)
			}
		})
	}
}

// ============================================================================
// withRetryHint：重答提醒的拼接
// ============================================================================

func TestWithRetryHint(t *testing.T) {
	const sys = "你是答题机器。"

	if got := withRetryHint(sys); got != sys {
		t.Errorf("不传提醒时应原样返回，得到 %q", got)
	}
	if got := withRetryHint(sys, ""); got != sys {
		t.Errorf("传空字符串时应原样返回，得到 %q", got)
	}
	if got := withRetryHint(sys, "   "); got != sys {
		t.Errorf("传全空白时应原样返回，得到 %q", got)
	}
	if got := withRetryHint(sys, "重答提醒"); got != sys+"重答提醒" {
		t.Errorf("应把提醒拼到末尾，得到 %q", got)
	}
	if got := withRetryHint(sys, "", "有效提醒"); got != sys+"有效提醒" {
		t.Errorf("应跳过空提醒取第一个有效值，得到 %q", got)
	}
}

// ============================================================================
// Prompt 配置：内置默认 + 外部覆盖
// ============================================================================

func TestDefaultPromptsAllNonEmpty(t *testing.T) {
	p := DefaultPrompts()
	fields := map[string]string{
		"AnswerChoice":   p.AnswerChoice,
		"AnswerMulti":    p.AnswerMulti,
		"Code":           p.Code,
		"ProgapCode":     p.ProgapCode,
		"ProgapBlanks":   p.ProgapBlanks,
		"ReanswerSuffix": p.ReanswerSuffix,
	}
	for name, v := range fields {
		if strings.TrimSpace(v) == "" {
			t.Errorf("%s 不应为空", name)
		}
	}
}

func TestDefaultPromptsContainFormatVerbs(t *testing.T) {
	// 这两个 Prompt 要用空的数量做格式化，占位符丢了就会输出字面量 %!d(MISSING)
	p := DefaultPrompts()
	if n := strings.Count(p.AnswerMulti, "%d"); n != 2 {
		t.Errorf("AnswerMulti 应有 2 个 %%d 占位符，实际 %d", n)
	}
	if n := strings.Count(p.ProgapBlanks, "%d"); n != 3 {
		t.Errorf("ProgapBlanks 应有 3 个 %%d 占位符，实际 %d", n)
	}
}

func TestLoadPromptsMissingFileFallsBack(t *testing.T) {
	// 文件不存在不该报错，直接回落默认值
	p := LoadPrompts(filepath.Join(t.TempDir(), "不存在.json"))
	if p.AnswerChoice != DefaultPrompts().AnswerChoice {
		t.Error("文件不存在时应返回内置默认值")
	}
	// 空路径同理
	if got := LoadPrompts(""); got.AnswerChoice != DefaultPrompts().AnswerChoice {
		t.Error("空路径时应返回内置默认值")
	}
}

func TestLoadPromptsPartialOverride(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "prompts.json")
	// 只覆盖一条，其余应保持默认
	content := `{"answer_choice": "自定义的单选提示"}`
	if err := os.WriteFile(file, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	p := LoadPrompts(file)
	def := DefaultPrompts()

	if p.AnswerChoice != "自定义的单选提示" {
		t.Errorf("AnswerChoice 应被覆盖，得到 %q", p.AnswerChoice)
	}
	if p.AnswerMulti != def.AnswerMulti {
		t.Error("未出现在文件里的字段应保持默认值（这是用 map 解析而非直接反序列化的原因）")
	}
	if p.ProgapBlanks != def.ProgapBlanks {
		t.Error("未出现在文件里的字段应保持默认值")
	}
}

func TestLoadPromptsIgnoresEmptyValues(t *testing.T) {
	// 空字符串不应把默认值覆盖成空——否则 Prompt 会彻底失效
	dir := t.TempDir()
	file := filepath.Join(dir, "prompts.json")
	if err := os.WriteFile(file, []byte(`{"answer_choice": ""}`), 0644); err != nil {
		t.Fatal(err)
	}
	if got := LoadPrompts(file); got.AnswerChoice != DefaultPrompts().AnswerChoice {
		t.Error("空值不应覆盖默认值")
	}
}

func TestLoadPromptsInvalidJSONFallsBack(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "prompts.json")
	if err := os.WriteFile(file, []byte(`{这不是合法 JSON`), 0644); err != nil {
		t.Fatal(err)
	}
	// 配置写错不应让程序起不来
	if got := LoadPrompts(file); got.AnswerChoice != DefaultPrompts().AnswerChoice {
		t.Error("JSON 非法时应回落默认值")
	}
}

func TestDumpPromptTemplateRoundTrip(t *testing.T) {
	b, err := DumpPromptTemplate()
	if err != nil {
		t.Fatalf("DumpPromptTemplate 出错: %v", err)
	}
	var p PromptSet
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatalf("生成的模板应能被解析回 PromptSet: %v", err)
	}
	if p != DefaultPrompts() {
		t.Error("模板内容应与内置默认值完全一致")
	}
}

func TestInitPromptsSetsGlobal(t *testing.T) {
	InitPrompts("")
	if Prompts.AnswerChoice == "" {
		t.Error("InitPrompts 应填充全局 Prompts")
	}
	// 恢复现场，避免影响其他用例
	Prompts = PromptSet{}
}
