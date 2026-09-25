package ai

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
)

// ============================================================================
// Prompt 集中管理。
//
// 改造前，5 个 System Prompt 硬编码在 ai/ai.go 与 progap.go 里，
// 想试试"换个说法能不能提高准确率"必须改代码重编译。
//
// 现在：内置一套默认值；若项目根目录存在 prompts.json，
// 则按字段覆盖（只写想改的那几条即可，其余保持默认）。
//
// 模板中形如 %d 的占位符由调用方在 Format 时填充。
// ============================================================================

// PromptSet 保存全部可覆盖的 System Prompt。
type PromptSet struct {
	// AnswerChoice 单选题：只输出一个选项字母
	AnswerChoice string `json:"answer_choice"`
	// AnswerMulti 多空题：每空一行，共 %d 行（占位符按出现顺序填入空的数量）
	AnswerMulti string `json:"answer_multi"`
	// Code 整段编程题：只输出纯代码
	Code string `json:"code"`
	// ProgapCode 程序片段题（整段代码）：补全后输出完整代码
	ProgapCode string `json:"progap_code"`
	// ProgapBlanks 程序片段题（逐空）：每空一行，共 %d 行
	ProgapBlanks string `json:"progap_blanks"`
	// ReanswerSuffix 重答时追加到原 System Prompt 末尾的提醒
	ReanswerSuffix string `json:"reanswer_suffix"`
}

// Prompts 是生效中的 Prompt 集合，由 InitAI 调用 LoadPrompts 填充。
var Prompts PromptSet

// DefaultPrompts 返回内置默认 Prompt（即改造前硬编码的那几套）。
func DefaultPrompts() PromptSet {
	return PromptSet{
		AnswerChoice: "你是答题机器。用户会给你一道单选题（可能含选项列表），" +
			"你只输出正确选项的大写字母（A、B、C、D、E、F 之一），" +
			"严禁输出任何其他字符、解释、标点或句号。",

		AnswerMulti: "你是答题机器。用户给你一道含 %d 个空的题目" +
			"（空以 ▁ 或 () 标记，选项以 A、B、C… 列出，每个空对应一个输入框）。\n" +
			"输出恰好 %d 行：第 i 行是第 i 个空的答案。\n" +
			"- 选择题的空：填正确选项字母；多选题把所有正确字母连写（如 ABD）。\n" +
			"- 填空题的空：填写空缺处应填的原文内容（代码、数值、单词等），保持简洁。\n" +
			"严禁输出编号、解释、引号或任何多余内容。",

		Code: "你是一个C语言编程专家。你的唯一任务是写代码。要求：" +
			"1.只输出纯粹的C语言源代码。" +
			"2.严禁输出Markdown标记。" +
			"3.不要任何解释、前言或后缀。" +
			"4.代码必须包含必要的头文件。" +
			"5.尤其注意格式问题,比如空格等",

		ProgapCode: "你是C语言程序填空题的答题机器。用户会给出题目正文" +
			"（含题目描述和待填空的代码片段，空白处用 ____ 或连续下划线标记）。\n" +
			"要求：\n" +
			"1. 只输出补全后的完整C语言代码，保持原有代码结构与缩进不变，仅把空白处填上。\n" +
			"2. 严禁输出 Markdown 代码围栏、解释、题号或任何多余文字。\n" +
			"3. 代码必须能直接编译通过（包含必要头文件由题目决定，不要擅自增删结构）。",

		ProgapBlanks: "你是C语言程序填空题的答题机器。题目正文里有 %d 个待填的空（对应 %d 个输入框）。\n" +
			"请输出恰好 %d 行：第 i 行是第 i 个空要填的内容" +
			"（只填该空本身的代码/表达式/数值，不要整段代码）。\n" +
			"严禁输出编号、解释、引号或多余内容。",

		ReanswerSuffix: "\n\n【重答提醒】这道题之前给出的答案已被判为不通过。" +
			"请重新审题，特别注意：边界条件、循环起止下标、变量类型与格式空格。" +
			"如果条件允许，请给出与上次不同的解法。",
	}
}

// LoadPrompts 加载 Prompt 配置。
//
// overwriteFile 为空或文件不存在时返回内置默认值（不算错误）。
// 文件存在但格式非法时打印警告并回退到默认值——绝不因为配置问题让程序起不来。
func LoadPrompts(overwriteFile string) PromptSet {
	p := DefaultPrompts()
	if overwriteFile == "" {
		return p
	}

	data, err := os.ReadFile(overwriteFile)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("读取 Prompt 配置文件失败，使用内置默认", "file", overwriteFile, "err", err)
		}
		return p
	}

	// 用 map 而非直接反序列化到 PromptSet：
	// 这样"只想改一条"时，未出现的字段会保持默认值，而不会被零值覆盖成空串。
	var ov map[string]string
	if err := json.Unmarshal(data, &ov); err != nil {
		slog.Warn("Prompt 配置文件格式非法，使用内置默认", "file", overwriteFile, "err", err)
		return p
	}

	apply := func(key string, dst *string) {
		if v, ok := ov[key]; ok && v != "" {
			*dst = v
		}
	}
	apply("answer_choice", &p.AnswerChoice)
	apply("answer_multi", &p.AnswerMulti)
	apply("code", &p.Code)
	apply("progap_code", &p.ProgapCode)
	apply("progap_blanks", &p.ProgapBlanks)
	apply("reanswer_suffix", &p.ReanswerSuffix)

	slog.Info("已加载 Prompt 配置", "file", overwriteFile, "keys", len(ov))
	return p
}

// DumpPromptTemplate 生成一份可直接编辑的 Prompt 模板（全部为内置默认值）。
// 供 CLI 的 -prompts-init 使用，方便用户从零开始改。
func DumpPromptTemplate() ([]byte, error) {
	b, err := json.MarshalIndent(DefaultPrompts(), "", "  ")
	if err != nil {
		return nil, fmt.Errorf("生成 Prompt 模板失败: %w", err)
	}
	return append(b, '\n'), nil
}
