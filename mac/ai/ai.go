package ai

import (
	"context"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"

	"github.com/cloudwego/eino-ext/components/model/ark"
	"github.com/cloudwego/eino/components/prompt"
	"github.com/cloudwego/eino/schema"
	"github.com/joho/godotenv"
)

var ChatModel *ark.ChatModel

// InitAI 初始化 AI 客户端
func InitAI() error {
	err := godotenv.Load()
	if err != nil {
		log.Println("警告: 没有找到 .env 文件")
	}

	ctx := context.Background()

	apiKey := os.Getenv("ARK_API_KEY")
	modelID := os.Getenv("ARK_MODEL_ID")

	fmt.Printf("正在初始化火山引擎, Model(Endpoint): %s\n", modelID)
	ChatModel, err = ark.NewChatModel(ctx, &ark.ChatModelConfig{
		APIKey: apiKey,
		Model:  modelID,
	})

	if err != nil {
		return err
	}
	return nil
}

func Answer(question string) (string, error) {

	template := prompt.FromMessages(schema.FString,
		schema.SystemMessage("你是一个{role}。"),
		&schema.Message{
			Role:    schema.User,
			Content: "请帮我{task}。",
		},
	)

	variables := map[string]any{
		"role": "C语言编程专家。你的唯一任务是写代码。要求：1.只输出纯粹的C语言源代码。2.严禁输出Markdown标记。3.不要任何解释、前言或后缀。4.代码必须包含必要的头文件。5.尤其注意格式问题,比如空格等",
		"task": question,
	}

	messages, err := template.Format(context.Background(), variables)
	if err != nil {
		return "", err
	}

	result, err := ChatModel.Generate(context.Background(), messages)
	if err != nil {
		log.Printf("生成失败，err:%v", err)
		return "", err
	}
	if len(result.Content) == 0 {
		return "", fmt.Errorf("模型返回内容为空")
	}

	rawContent := CleanCode(result.Content)
	return rawContent, nil

}

// codeFenceRe 匹配形如 ```c / ```cpp / ``` 的代码围栏及其后的换行
var codeFenceRe = regexp.MustCompile("(?s)```[a-zA-Z0-9+#]*\\n?")

func CleanCode(input string) string {
	input = codeFenceRe.ReplaceAllString(input, "")
	return strings.TrimSpace(input)
}

// AnswerChoice 单选填空题：从题干（含选项）生成一个选项字母 A-D
func AnswerChoice(question string) (string, error) {
	messages := []*schema.Message{
		schema.SystemMessage("你是答题机器。用户会给你一道单选题（可能含选项列表），你只输出正确选项的大写字母（A、B、C、D、E、F 之一），严禁输出任何其他字符、解释、标点或句号。"),
		{Role: schema.User, Content: question},
	}
	result, err := ChatModel.Generate(context.Background(), messages)
	if err != nil {
		return "", err
	}
	if len(result.Content) == 0 {
		return "", fmt.Errorf("模型返回内容为空")
	}
	ans := strings.ToUpper(strings.TrimSpace(CleanCode(result.Content)))
	for _, r := range ans {
		if r >= 'A' && r <= 'Z' {
			return string(r), nil
		}
	}
	return "", fmt.Errorf("无法从模型输出中提取选项字母: %q", ans)
}

// AnswerMulti 多选/多空题：题干含 n 个空，返回 n 个答案（按空顺序）。
// 选择题的空填选项字母（多选题所有正确字母连写如 "ABD"）；填空题的空填原文内容。
func AnswerMulti(question string, n int) ([]string, error) {
	sys := fmt.Sprintf(`你是答题机器。用户给你一道含 %d 个空的题目（空以 ▁ 或 () 标记，选项以 A、B、C… 列出，每个空对应一个输入框）。
输出恰好 %d 行：第 i 行是第 i 个空的答案。
- 选择题的空：填正确选项字母；多选题把所有正确字母连写（如 ABD）。
- 填空题的空：填写空缺处应填的原文内容（代码、数值、单词等），保持简洁。
严禁输出编号、解释、引号或任何多余内容。`, n, n)
	messages := []*schema.Message{
		schema.SystemMessage(sys),
		{Role: schema.User, Content: question},
	}
	result, err := ChatModel.Generate(context.Background(), messages)
	if err != nil {
		return nil, err
	}
	if len(result.Content) == 0 {
		return nil, fmt.Errorf("模型返回内容为空")
	}
	var lines []string
	for _, ln := range strings.Split(CleanCode(result.Content), "\n") {
		ln = strings.TrimSpace(ln)
		if ln != "" {
			lines = append(lines, ln)
		}
	}
	if len(lines) != n {
		return nil, fmt.Errorf("模型输出 %d 行，期望 %d 行: %q", len(lines), n, lines)
	}
	return lines, nil
}
