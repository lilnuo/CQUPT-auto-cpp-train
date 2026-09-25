# 重庆邮电大学程序设计平台自动刷题脚本

> **版本：v1.2.0（2026-09-24）** ｜ **平台：macOS** ｜ 更新日志见根目录 [CHANGELOG.md](../CHANGELOG.md)

> ⚠️ **仅限个人学习交流，严禁用于任何盈利目的**；使用风险自负（详见根目录 [README](../README.md) 的免责声明）。
> ⭐ 觉得好用的话，欢迎给仓库点个 **Star**。

你是否也被学校无用的 C++ 语言刷题所困扰？此脚本可以帮助你解决烦恼。

适配站点：https://prg.cqupt.edu.cn （含瑞数 WAF 防护，脚本已自动绕过）

## 功能

| 题型 | 命令 | 说明 |
|------|------|------|
| 选择/填空题（整页内嵌题） | `go run .` | 自动识别选项字母/填空内容，自动提交 |
| 程序片段编程题 | `go run . -mode=progap` | AI 补全代码中嵌的空，自动提交并回读判题结果 |

全流程自动：启动浏览器 → 过 WAF 挑战 → 识别验证码登录（ddddocr）→ 选择作业卡 → 逐题作答 → 自动提交 → **读回得分**。中断后重跑会跳过已提交的题。

**v1.2.0 新增**：提交前后各读一次页面总分，差值即该题得分；没得分可按策略重答（同页重填，程序题重新打开题目页）。次数用尽仍不得分的题记入 `wrong_answers.md`。

## 前置条件

1. 安装 Go 和 Chrome 浏览器
2. 在 [火山引擎](https://console.volcengine.com/ark) 开通服务领取免费额度，创建 API Key 和模型接入点
3. 验证码识别依赖 ddddocr（Python）：
   ```bash
   pip install ddddocr
   ```
   脚本会依次尝试 `PYTHON_BIN` 环境变量 → 本机 venv → 系统 `python3` 来调用它。
   你也可以在 `ai` 包里换别家模型（模型太弱可能得 0 分）。

## 使用方法

```bash
# 1. 配置凭据：复制模板并填入你自己的 Key
cp .env.example .env
# 然后编辑 .env，填入 ARK_API_KEY 和 ARK_MODEL_ID

# 2. 刷选择/填空题
go run .

# 3. 刷程序片段编程题
go run . -mode=progap

# 4. （可选）生成 Prompt 模板，方便不改代码调提示词
go run . -prompts-init
```

按提示输入学号、密码、想刷的题目数量（建议先填 1~3 验证，确认没问题再放量）。

## 项目结构

```
main.go        # 登录 + 过 WAF + 选择/填空题流程 + 得分闭环
progap.go      # 程序片段编程题流程 + 判题回显解析
main_test.go   # 单测
config/        # ★ 全部可调项的集中地
  config.go    #   环境变量读取 + 重答策略
  site.go      #   站点选择器/XPath/正则/文案 + 所有超时参数
  env.go       #   平台差异唯一来源（Chrome 路径 / Python 探测 / profile 目录）
ai/            # 大模型调用 + Prompt 管理（prompts.go）
ocr/           # ddddocr 验证码识别
tools/         # 排查用的小工具（aitest / attachtest / logindump / wafprobe）
```

**站点改版了就改 `config/site.go`，其余代码基本不用动。**

## 可选参数 / 环境变量

| 项 | 默认 | 作用 |
|----|------|------|
| `-mode=quiz` | — | 默认，整页选择填空题 |
| `-mode=progap` | — | 程序片段编程题 |
| `-mode=progapdump` | — | 只 dump 第一道程序题页面到 progap.html（调试用） |
| `-dump` | — | quiz 模式下 dump 答题页到 page.html（调试用） |
| `-log-json` | — | 日志改输出 JSON 格式 |
| `-prompts-init` | — | 生成 prompts.example.json 后退出 |
| `ASSIGN_KEYWORD` | `刷题` | 有多张作业卡时按标题关键词选卡 |
| `CHROME_PATH` | 自动探测 | 自定义 Chrome 路径 |
| `CHROME_USER_DATA` | `.chrome-profile` | 浏览器 profile 目录 |
| `CDP_PORT` | `9223` | 调试端口 |
| `CDP_URL` | 空 | 接入已开着的浏览器，跳过自动登录 |
| `PYTHON_BIN` | 自动探测 | 自定义 Python 解释器 |
| `REANSWER_THRESHOLD` | `0` | 得分 ≤ 此值即重答；`-1` = 从不重答 |
| `MAX_ANSWER_TRY` | `2` | 单题最多作答次数（含首次），最小 1 |
| `PROMPTS_FILE` | `prompts.json` | Prompt 覆盖文件路径 |
| `LOG_FORMAT` | `text` | 日志格式：`text` / `json` |

> 想要「和旧版 v1.1.1 完全一样」的行为：设 `MAX_ANSWER_TRY=1` 或 `REANSWER_THRESHOLD=-1`。

## 调 Prompt 不用改代码

```bash
go run . -prompts-init       # 生成 prompts.example.json
mv prompts.example.json prompts.json
# 只写想改的那几条 key，其余不写即用内置默认
go run .
```

## 开发 / 自测

```bash
go build ./...     # 编译
go vet ./...       # 静态检查
go test ./...      # 单测（99 个用例）
```

## 常见问题

- **验证码识别失败**：脚本会自动重试 4 次，仍失败会停下来让你在弹出的浏览器里手动登录，登录后自动继续。
- **白屏/页面为空**：站点有瑞数 WAF，脚本采用"原生启动 Chrome 过挑战后再接管"的方式绕过，启动后请耐心等待约 20 秒，不要去动弹出的浏览器窗口。
- **想换作业**：设 `ASSIGN_KEYWORD=作业标题里的关键词`。
- **想省 token**：设 `MAX_ANSWER_TRY=1`，只答一次不重试。

## 效果图

![show](img/show.png)
