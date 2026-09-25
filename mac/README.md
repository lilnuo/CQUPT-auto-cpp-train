# 重庆邮电大学程序设计平台自动刷题脚本

> **版本：v1.3.0（2026-09-25）** ｜ **平台：macOS** ｜ 更新日志见根目录 [CHANGELOG.md](../CHANGELOG.md)

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

**v1.3.0 新增**：可选的**服务模式**（`go run ./server`，MySQL 队列 + 后台 worker + 进度页），命令行用法完全没变；另外启动浏览器前会先检查 profile / 调试端口是否被另一个 Chrome 占着，占着就直接报错，不再"想办法连上"（那条路会连到别人的浏览器，把 WAF 绕过的前提破坏掉，且**不报错**）。详见下方「服务模式」。

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
main.go        # CLI 入口（薄壳）：解析参数、交互式问答、打印结果
train/         # ★ 刷题流程本体（v1.3.0 从 main 包抽出来，服务端复用这一层）
  api.go       #   对外接口：Run / Request / Result / 进度事件 / 哨兵错误
  train.go     #   登录 + 过 WAF + 选择/填空题 + 得分闭环
  progap.go    #   程序片段编程题 + 判题回显解析
  preflight.go #   启动前的占用闸门（端口 / profile 锁探测）
config/        # ★ 全部可调项的集中地
  config.go    #   环境变量读取 + 重答策略
  site.go      #   站点选择器/XPath/正则/文案 + 所有超时参数
  env.go       #   平台差异唯一来源（Chrome 路径 / Python 探测 / profile 目录）
ai/            # 大模型调用 + Prompt 管理（prompts.go）
ocr/           # ddddocr 验证码识别
server/        # 服务模式（可选）：HTTP + MySQL 队列 + worker + 进度页
tools/         # 排查用的小工具（cdpdiag / aitest / attachtest / logindump / wafprobe）
```

**站点改版了就改 `config/site.go`，其余代码基本不用动。**

`train` 之所以单独成包：Go **不允许导入 `package main`**，服务端要复用刷题流程，
流程就得先离开 `main` 包。「能跑起来的脚本」和「能被别的代码调用的模块」是两回事——
脚本把输入读自 stdin、结果打到终端、用退出码表示成败；模块必须把输入、输出、
错误三样都变成显式的参数与返回值。

## 服务模式（可选）

命令行用法没变。这一节是给「让机器排队慢慢刷」准备的——一次刷题要独占一个 Chrome、
一个 profile、一个调试端口，所以一台机器上并发不了，想让多个人提交任务就得排队。

```bash
mysql -uroot -p < server/schema.sql     # 建表 + 建只给 DML 权限的应用账号
cat > server.env <<'EOF'
TASK_SECRET=至少32个字符的随机串（openssl rand -hex 32）
DB_PASSWORD=改成你自己的口令
EOF
go run ./server                         # 默认监听 127.0.0.1:8080
```

提交任务用 `POST /api/tasks`，进度页在 `GET /`。完整的接口表、配置项、
可靠性机制（重试退避 / 续租心跳 / 僵死回收 / 优雅退出）与已知限制见 [`server/README.md`](./server/README.md)。

> ⚠️ 进度页**没有鉴权**，且默认只监听 `127.0.0.1`。要对外暴露必须先加认证。

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
go test ./...      # 单测（86 个测试函数 / 148 个用例，默认跳过需要数据库的部分）
```

需要 MySQL 的那部分测试要显式给 DSN 才会跑（不给就自动跳过）：

```bash
CQUPT_TEST_DSN='cqupt_app:口令@tcp(127.0.0.1:3306)/cqupt_train?parseTime=true&loc=Local&charset=utf8mb4' go test ./...
```

## 常见问题

- **验证码识别失败**：脚本会自动重试 4 次，仍失败会停下来让你在弹出的浏览器里手动登录，登录后自动继续。
- **白屏/页面为空**：站点有瑞数 WAF，脚本采用"原生启动 Chrome 过挑战后再接管"的方式绕过，启动后请耐心等待约 20 秒，不要去动弹出的浏览器窗口。
- **报错「建立浏览器控制连接失败」**：先跑 `go run ./tools/cdpdiag -headless`（不弹窗口，逐层报告卡在哪一步）。
  最常见的一类成因是 **profile 或调试端口被另一个 Chrome 占着**——现在程序会在启动浏览器**之前**就拦下这种情况并直接报错，
  提示你退出所有 Chrome，或用 `CDP_PORT` / `CHROME_USER_DATA` 换一组。
- **想换作业**：设 `ASSIGN_KEYWORD=作业标题里的关键词`。
- **想省 token**：设 `MAX_ANSWER_TRY=1`，只答一次不重试。

## 效果图

![show](img/show.png)
