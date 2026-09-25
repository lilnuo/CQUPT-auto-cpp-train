package ai

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"cqupt/config"

	"github.com/cloudwego/eino-ext/components/model/ark"
	"github.com/cloudwego/eino/schema"
)

// ChatModel 是全局模型句柄，由 InitAI 初始化。
var ChatModel *ark.ChatModel

// InitPrompts 加载 Prompt 配置到全局 Prompts。
//
// 单独抽出来是为了与 InitAI 解耦：dump 模式不需要模型，
// 但流程里仍可能用到 Prompt，所以 Prompt 的加载要无条件执行。
func InitPrompts(file string) {
	Prompts = LoadPrompts(file)
}

// InitAI 初始化 AI 客户端。
// 配置来源已收拢到 config 包（含 .env 的加载），本函数不再自己读环境变量。
func InitAI() error {
	if config.C.APIKey == "" || config.C.ModelID == "" {
		return fmt.Errorf("缺少 ARK_API_KEY 或 ARK_MODEL_ID，请检查 .env")
	}
	slog.Info("正在初始化火山引擎", "model", config.C.ModelID)

	m, err := ark.NewChatModel(context.Background(), &ark.ChatModelConfig{
		APIKey: config.C.APIKey,
		Model:  config.C.ModelID,
	})
	if err != nil {
		return err
	}
	ChatModel = m
	return nil
}

// ============================================================================
// 底层调用
// ============================================================================

// generate 发起一次对话补全。所有能力都走这里，便于统一日志与错误处理。
func generate(sys, user string) (string, error) {
	if ChatModel == nil {
		return "", fmt.Errorf("AI 未初始化（请先调用 InitAI）")
	}
	msgs := []*schema.Message{
		schema.SystemMessage(sys),
		{Role: schema.User, Content: user},
	}
	res, err := ChatModel.Generate(context.Background(), msgs)
	if err != nil {
		slog.Error("模型调用失败", "err", err)
		return "", err
	}
	if res == nil || strings.TrimSpace(res.Content) == "" {
		return "", fmt.Errorf("模型返回内容为空")
	}
	return res.Content, nil
}

// withRetryHint 把重答提醒拼到 System Prompt 末尾。
//
// 首次作答不传 extra，行为与改造前完全一致；
// 重答时调用方传入 Prompts.ReanswerSuffix，让模型换个思路再想一遍。
func withRetryHint(sys string, extra ...string) string {
	for _, e := range extra {
		if strings.TrimSpace(e) != "" {
			return sys + e
		}
	}
	return sys
}

// ============================================================================
// 代码清洗与文本提取
// ============================================================================

// codeFenceRe 匹配形如 ```c / ```cpp / ``` 的代码围栏及其后的换行
var codeFenceRe = regexp.MustCompile("(?s)```[a-zA-Z0-9+#]*\\n?")

// CleanCode 去掉 Markdown 代码围栏与首尾空白。
func CleanCode(input string) string {
	input = codeFenceRe.ReplaceAllString(input, "")
	return strings.TrimSpace(input)
}

// firstUpperLetter 从模型输出里取出第一个大写英文字母（A-Z）。
// 这样即使模型多吐了句号或解释，也能稳妥拿到选项字母。
func firstUpperLetter(s string) (string, bool) {
	for _, r := range strings.ToUpper(strings.TrimSpace(s)) {
		if r >= 'A' && r <= 'Z' {
			return string(r), true
		}
	}
	return "", false
}

// SplitNonEmptyLines 按行切分并去掉空行，用于"每空一行"的答案解析。
// 导出是为了让测试能直接验证解析行为。
func SplitNonEmptyLines(s string) []string {
	var lines []string
	for _, ln := range strings.Split(CleanCode(s), "\n") {
		if ln = strings.TrimSpace(ln); ln != "" {
			lines = append(lines, ln)
		}
	}
	return lines
}

// ============================================================================
// 对外能力
// ============================================================================

// Answer 整段编程题：输入题干，返回可提交的代码（已去围栏）。
// 本题型对应站点上的 Monaco 编辑器题。
func Answer(question string, extra ...string) (string, error) {
	content, err := generate(withRetryHint(Prompts.Code, extra...), question)
	if err != nil {
		return "", err
	}
	return CleanCode(content), nil
}

// AnswerChoice 单选题：从题干（含选项）生成一个选项字母。
func AnswerChoice(question string, extra ...string) (string, error) {
	content, err := generate(withRetryHint(Prompts.AnswerChoice, extra...), question)
	if err != nil {
		return "", err
	}
	letter, ok := firstUpperLetter(content)
	if !ok {
		return "", fmt.Errorf("无法从模型输出中提取选项字母: %q", CleanCode(content))
	}
	return letter, nil
}

// AnswerMulti 多空题：题干含 n 个空，返回 n 个答案（按空顺序）。
// 选择题的空填选项字母（多选连写如 "ABD"）；填空题的空填原文内容。
func AnswerMulti(question string, n int, extra ...string) ([]string, error) {
	sys := fmt.Sprintf(Prompts.AnswerMulti, n, n)
	content, err := generate(withRetryHint(sys, extra...), question)
	if err != nil {
		return nil, err
	}
	lines := SplitNonEmptyLines(content)
	if len(lines) != n {
		return nil, fmt.Errorf("模型输出 %d 行，期望 %d 行: %q", len(lines), n, lines)
	}
	return lines, nil
}

// AnswerBlanks 逐空填空（程序片段题的 textarea 版本）：
// 题干里有 n 个空，返回 n 行答案，每行只填该空本身的内容。
func AnswerBlanks(body string, n int, extra ...string) ([]string, error) {
	sys := fmt.Sprintf(Prompts.ProgapBlanks, n, n, n)
	content, err := generate(withRetryHint(sys, extra...), body)
	if err != nil {
		return nil, err
	}
	lines := SplitNonEmptyLines(content)
	if len(lines) != n {
		return nil, fmt.Errorf("模型输出 %d 行，期望 %d 行: %q", len(lines), n, lines)
	}
	return lines, nil
}

// AnswerCode 程序片段题的"整段代码"版本：返回补全后的完整代码。
func AnswerCode(body string, extra ...string) (string, error) {
	content, err := generate(withRetryHint(Prompts.ProgapCode, extra...), body)
	if err != nil {
		return "", err
	}
	code := CleanCode(content)
	if code == "" {
		return "", fmt.Errorf("模型返回代码为空")
	}
	return code, nil
}
