// Package config 集中管理运行期配置。
//
// 设计目标：把原先散落在 main.go / progap.go / ai/ai.go 里的
// os.Getenv、选择器字面量、魔法数字统统收拢到本包，
// 将来换模型、调 Prompt、站点改版时只需改这里，不必翻遍全项目。
//
// 本包只依赖标准库与 godotenv，不依赖包内其他代码，可被任何层安全导入。
package config

import (
	"os"
	"strconv"
	"strings"

	"github.com/joho/godotenv"
)

// Config 保存全部运行期可配置项。
type Config struct {
	// ---- 大模型（火山引擎方舟）----
	APIKey  string
	ModelID string

	// ---- 浏览器 ----
	ChromePath     string // 为空则自动探测
	ChromeUserData string // 浏览器 profile 目录，登录态持久化于此
	CDPPort        string
	CDPURL         string // 非空则直接接入已打开的浏览器，跳过自动登录

	// ---- 站点交互 ----
	AssignKeyword string // 作业卡标题关键词

	// ---- 验证码识别 ----
	PythonBin string // 为空则自动探测

	// ---- 行为策略 ----
	ReanswerThreshold float64 // 该题得分 <= 此值时重答；设为 -1 表示从不重答
	MaxAnswerTry      int     // 单题最多作答次数（含首次）
	PromptsFile       string  // Prompt 覆盖文件路径；为空则不加载
	LogFormat         string  // 日志格式：text / json

	// ---- 命令行 ----
	Dump bool   // 进入做题页后 dump 页面结构
	Mode string // 运行模式：quiz / progap / progapdump
}

// C 是全局配置，由 Load 在程序启动时初始化。
// 之所以用包级变量而非依赖注入，是为了不改变既有函数的签名，
// 把改造范围限制在"取值来源"这一层，从而不触碰已跑通的流程逻辑。
var C Config

// Load 读取 .env 与环境变量，填充全局配置并返回。
func Load() Config {
	// .env 是可选的：不存在时视为"用户直接用环境变量"，不算错误
	_ = godotenv.Load()

	C = Config{
		APIKey:            os.Getenv("ARK_API_KEY"),
		ModelID:           os.Getenv("ARK_MODEL_ID"),
		ChromePath:        os.Getenv("CHROME_PATH"),
		ChromeUserData:    envOr("CHROME_USER_DATA", DefaultChromeProfile),
		CDPPort:           envOr("CDP_PORT", DefaultCDPPort),
		CDPURL:            os.Getenv("CDP_URL"),
		AssignKeyword:     envOr("ASSIGN_KEYWORD", DefaultAssignKeyword),
		PythonBin:         os.Getenv("PYTHON_BIN"),
		ReanswerThreshold: envFloat("REANSWER_THRESHOLD", DefaultReanswerThreshold),
		MaxAnswerTry:      envInt("MAX_ANSWER_TRY", DefaultMaxAnswerTry),
		PromptsFile:       envOr("PROMPTS_FILE", DefaultPromptsFile),
		LogFormat:         strings.ToLower(envOr("LOG_FORMAT", "text")),
	}
	// 至少得试一次，否则流程会一步都走不下去
	if C.MaxAnswerTry < 1 {
		C.MaxAnswerTry = 1
	}
	return C
}

// ShouldReanswer 判断某题得分是否达到"需要重答"的程度。
//
// 语义：得分 <= 阈值 即重答。
//   - 阈值取默认值 0 时，只有"一分未得"才重答，最保守；
//   - 阈值取 -1 时永不重答，等价于改造前的行为；
//   - 阈值取 2.5 时，低于 2.5 分就重答，最激进。
//
// try 是已尝试次数（含本次），达到 MaxAnswerTry 后不再重答。
func (c Config) ShouldReanswer(score float64, try int) bool {
	if c.ReanswerThreshold < 0 {
		return false
	}
	if try >= c.MaxAnswerTry {
		return false
	}
	return score <= c.ReanswerThreshold
}

// IsJSONLog 报告是否应输出 JSON 格式日志。
func (c Config) IsJSONLog() bool { return c.LogFormat == "json" }

func envOr(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func envFloat(key string, def float64) float64 {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}
