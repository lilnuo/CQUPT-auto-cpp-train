package config

import "time"

// ============================================================================
// 本文件是**目标站点的 DOM 契约与时间参数的唯一来源**。
//
// 站点改版时，理论上只需要改这一个文件：
//   1. 页面结构变了 → 改下面的选择器常量；
//   2. 网站变慢/变快    → 改下面的超时常量；
//   3. 想换作业卡/调策略 → 改下面的默认值常量（或用环境变量覆盖）。
//
// naming 约定：Sel* = CSS/XPath 选择器；URL* = 地址；Text* = 用于匹配的文本；
//             Re* = 正则；Timeout*/Poll*/Wait* = 时间参数；Default* = 默认值。
// ============================================================================

// ---------------- 站点地址 ----------------

const (
	// URLSiteRoot 站点根地址
	URLSiteRoot = "https://prg.cqupt.edu.cn"
	// URLLogin 登录页（simple.jsp）
	URLLogin = URLSiteRoot + "/indexcs/simple.jsp?loginErr=0"
	// URLMainPage 登录后落地的主页（作业卡片列表），用于判断是否已进入答题流程
	URLMainPage = "main.jsp"
)

// ---------------- 登录页选择器 ----------------

const (
	SelUsername    = "#username"
	SelPassword    = "#password"
	SelCaptchaCode = "#captchaCode"
	// SelCaptchaImg 验证码图片；点击它可刷新出新验证码
	SelCaptchaImg = `img[src="/cgjiaoyan"]`
	SelLoginBtn   = "#cgstuloginbtn"
	// TextLoginErrFlag 登录失败时 URL 会带上这个标记
	TextLoginErrFlag = "loginErr=1"
)

// ---------------- 编程题（Monaco 编辑器）选择器 ----------------

const (
	SelCodeQuestion = ".q-content"
	SelMonaco       = ".monaco-editor"
	SelScoreCell    = `tr[role="row"] td.mat-column-score`

	XPathSubmitBtn  = `//button[contains(., "提交")]`
	XPathConfirmBtn = `//button[contains(., "确定")]`
)

// ---------------- 整页选择/填空题选择器 ----------------

const (
	// SelQuizForm 每道题包在一个 form 里，name = answerForm{题号}
	SelQuizForm = `form[name^="answerForm"]`
	// SelQuizInput 题内每个空对应一个 input，name = answer{N}
	SelQuizInput = `input[name^="answer"]`
	// SelQuizSaveTipPrefix 提交状态提示元素 id 前缀，完整 id 形如 saveTip123
	SelQuizSaveTipPrefix = "saveTip"
	// TextQuizSubmitted 状态提示出现该文本即视为已提交
	TextQuizSubmitted = "已提交"

	// ReQuizPid 从 form.name 中取出题号
	ReQuizPid = `answerForm(\d+)`
	// ReTotalScoreJS 从页面正文中取出 "总分: x.xx"，在浏览器端执行
	ReTotalScoreJS = `总分[:：]\s*([\d.]+)`
	// ReTotalScore 与上式等价，用于 Go 侧解析
	ReTotalScore = `总分[:：]\s*([\d.]+)`

	// TextQuizMarkers 页面出现这些词即判定为选择/填空题页
	TextQuizMarkers = "单选填空|单项选择题"
)

// ---------------- 程序片段编程题选择器 ----------------

const (
	// SelProgapListLink 作业页上指向程序题的链接
	SelProgapListLink = `a[href*="programFillGapList"]`
	// SelProgapCodeArea 代码区容器（填空用的 textarea 嵌在其中）
	SelProgapCodeArea = "#cgsoucecode"
	// SelProgapForm 提交用的表单，action = showProcessMsg.jsp
	SelProgapForm = `form[name="uploadFORM"]`
	// SelProgapAnswer 每个空对应一个 textarea，name = answer{N}
	SelProgapAnswer = `textarea[name^="answer"]`
	// SelProgapSubmitBtn 提交按钮。
	// 注意：其可见文本是"提 交"（中间有空格），不能按文本匹配，只能按 ID。
	SelProgapSubmitBtn = "#cgSubmitBtn"
	// SelProgapResultFrame 判题结果回显在隐藏 iframe 里
	SelProgapResultFrame = `iframe[name="showmessageFRAME"]`

	// SelAnyLinkButton 泛化的可点击元素，用于作业卡枚举
	SelAnyLinkButton = "a,button"
	// ReAssignmentBtn 作业卡上"进入作业"按钮的文本特征
	ReAssignmentBtn = `进入作业|开始答题|进入答题`
	// ReAssignmentNoise 枚举作业卡时需过滤掉的噪声行
	ReAssignmentNoise = `进入作业|开始答题|作业时间|截止`

	// TextJudging 判题进行中的关键词，出现这些说明结果还没出来
	TextJudging  = "正在"
	TextJudging2 = "处理中"
	TextJudging3 = "Loading"

	// ReProgapFailKeywords 判题结果里出现这些词，视为"没通过"。
	// 站点回显的文案没有稳定契约，所以用关键词集合而不是精确匹配；
	// 拿不准时宁可判成"通过"——漏判只是少重答一次，误判会白白多花 token。
	// 加 (?i) 是为了兼容英文回显（Wrong / Error / Failed）；中文不受影响。
	ReProgapFailKeywords = `(?i)错误|失败|不通过|未通过|wrong|error|fail`
	// ReProgapPassKeywords 判题结果里出现这些词，视为"通过"
	ReProgapPassKeywords = `(?i)正确|通过|恭喜|accept`

	// ReProgapNotSubmitted 程序题列表里"还没提交"的状态文本
	ReProgapNotSubmitted = `还未提交|未提交`
)

// ---------------- 时间参数 ----------------

const (
	// ---- 启动与 WAF ----
	TimeoutChromeWSWait = 15 * time.Second // 等 Chrome 在 stderr 打印 DevTools 地址
	WaitWAFChallenge    = 20 * time.Second // 等瑞数挑战自行通过（原生标签页无干扰）
	TimeoutLoginNav     = 45 * time.Second // 导航到登录页并等加载
	WaitLoginNavRetry   = 10 * time.Second // 登录页没就绪时的重试间隔
	MaxLoginNavRetry    = 3

	// ---- 登录 ----
	TimeoutLoginFormReady = 20 * time.Second
	TimeoutCaptchaShot    = 15 * time.Second
	TimeoutLoginSubmit    = 10 * time.Second
	PollLoginResultMax    = 10                      // 登录结果轮询次数上限
	PollLoginResultTick   = 800 * time.Millisecond  // 登录结果轮询间隔
	WaitCaptchaRefresh    = 1500 * time.Millisecond // 点验证码后等新图
	MaxLoginTry           = 4                       // OCR 最多尝试次数
	PollExistingLoginMax  = 8                       // 检测"已有登录态"的轮询次数
	PollExistingLoginTick = 2 * time.Second
	PollManualLoginTick   = 2 * time.Second
	TimeoutManualLogin    = 10 * time.Minute // 人工兜底最长等待

	// ---- 作业卡 ----
	PollAssignmentMax     = 20 // 等作业卡渲染出来的轮询次数
	PollAssignmentTick    = 2 * time.Second
	TimeoutAssignmentPoll = 8 * time.Second  // 单次枚举作业卡的超时
	TimeoutAssignClick    = 10 * time.Second // 点击"进入作业"
	PollAssignResultMax   = 15               // 等跳转结果的轮询次数
	PollAssignResultTick  = 1 * time.Second

	// ---- 整页选择/填空题 ----
	TimeoutQuizPage       = 60 * time.Minute
	PollQuizListMax       = 30 // 等题目表单渲染的轮询次数
	PollQuizListTick      = 2 * time.Second
	PollQuizSubmittedMax  = 8 // 等自动提交生效的轮询次数
	PollQuizSubmittedTick = 1 * time.Second
	WaitBeforeReadScore   = 2 * time.Second
	// WaitScoreChange 提交后等页面总分更新的窗口。
	// 服务器算分需要时间，窗口太短会把"还没算完"误判成"得了 0 分"进而误触发重答。
	WaitScoreChange = 6 * time.Second
	// PollScoreTick 等总分更新时的轮询间隔
	PollScoreTick = 1 * time.Second

	// ---- 编程题（Monaco）----
	TimeoutCodeQuestion = 120 * time.Second

	// ---- 页面渲染通用 ----
	PollRenderMax       = 15 // 等页面动态内容的轮询次数
	PollRenderTick      = 2 * time.Second
	RenderTextThreshold = 30 // 正文长度超过该值视为渲染完成
	TimeoutNavPollTick  = 8 * time.Second

	// ---- 程序片段编程题 ----
	TimeoutProgapQuestion = 3 * time.Minute
	TimeoutProgapListNav  = 60 * time.Second
	TimeoutProgapSubmit   = 10 * time.Second
	TimeoutProgapConfirm  = 6 * time.Second
	PollProgapRenderMax   = 15
	PollProgapJudgeMax    = 30 // 判题结果轮询次数（服务器跑用例可能较慢）
	PollProgapJudgeTick   = 2 * time.Second
	TimeoutProgapBack     = 30 * time.Second
	WaitProgapBackToList  = 2 * time.Second
	WaitConfirmDialog     = 1500 * time.Millisecond
	WaitAfterSubmit       = 3 * time.Second
	TimeoutProgapProbe    = 15 * time.Second
	TimeoutSingleAction   = 10 * time.Second

	// ---- dump ----
	TimeoutDump = 90 * time.Second
	PollDumpMax = 15
)

// ---------------- 默认值 ----------------

const (
	// DefaultCDPPort Chrome 远程调试端口
	DefaultCDPPort = "9223"
	// DefaultChromeProfile 浏览器 profile 目录名（登录态持久化于此）
	DefaultChromeProfile = ".chrome-profile"
	// DefaultChromeProfilePrefix 调试工具用的 profile 前缀
	DefaultChromeProfilePrefix = ".chrome-profile-"
	// DefaultAssignKeyword 作业卡标题关键词
	DefaultAssignKeyword = "刷题"
	// DefaultPromptsFile Prompt 覆盖文件
	DefaultPromptsFile = "prompts.json"
	// DefaultReanswerThreshold 得分 <= 此值即重答。0 = 只有一分未得才重答
	DefaultReanswerThreshold = 0
	// DefaultMaxAnswerTry 单题最多作答次数。设为 1 等价于改造前的行为
	DefaultMaxAnswerTry = 2
	// DefaultLogFormat 日志格式
	DefaultLogFormat = "text"
)

// ---------------- 调试产物路径 ----------------

const (
	// FileDumpQuiz 选择题页 dump 文件名
	FileDumpQuiz = "page.html"
	// FileDumpProgap 程序题页 dump 文件名
	FileDumpProgap = "progap.html"
	// FileWrongAnswers 答错题目的复盘记录
	FileWrongAnswers = "wrong_answers.md"
	// FileCaptchaTemp 验证码截图临时文件名
	FileCaptchaTemp = "cqupt_captcha.png"
	// DirOcrScript 验证码识别脚本所在目录
	DirOcrScript = "ocr"
	// FileOcrScript 验证码识别脚本文件名
	FileOcrScript = "ocr_captcha.py"
)
