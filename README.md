# 重庆邮电大学程序设计平台自动刷题脚本

> 适配站点：https://prg.cqupt.edu.cn （含瑞数 WAF 防护，脚本已自动绕过）

一个能**自动登录 + 自动做题 + 回读得分 + 答错重试**的脚本：启动浏览器 → 过 WAF → OCR 识别验证码登录 → 选作业卡 → 逐题 AI 作答 → 自动提交 → 读回分数，没得分就换思路重答。

当前版本：**v1.3.0**（见 [VERSION](./VERSION) / [CHANGELOG.md](./CHANGELOG.md)）

> v1.3.0 新增**可选的服务模式**：命令行用法没变，另提供一条 HTTP 服务入口（MySQL 队列 + 后台 worker + 进度页），适合「多个人提交任务、机器排队慢慢刷」。见下方[服务模式](#服务模式可选v130-新增)。

---

## ⚠️ 风险提示与免责声明（请务必阅读）

- **仅供个人学习交流使用，严禁用于任何盈利目的**。包括但不限于：付费代做、售卖脚本或账号、引流收费、打包成商品分发等，一经发现请勿使用本项目。
- 脚本通过模拟真人操作自动登录并答题，**使用前请自行评估风险**：这可能违反学校/平台的相关规定，存在账号异常、成绩不予认定甚至纪律处理等后果，**一切后果由使用者自行承担**。
- 请**合理使用**。刷题只是应付作业的手段，C++ 与算法基础还是得自己动手写才真正掌握——别让它替代你的学习。
- 本项目按「现状」提供，作者不对使用本脚本造成的任何直接或间接损失负责。

## ⭐ 求个 Star

如果这个脚本帮你省下了时间，欢迎在仓库右上角点个 **Star ⭐** 支持一下。
你的 star 就是作者继续维护、适配新题型的动力，感谢 🙏

---

## 平台版本对照

本仓库同时收录两个平台版本，核心逻辑一致，仅浏览器路径 / Python 命令解析按系统适配（差异统一收敛在各自的 `config/env.go` 里）。**请按你的系统进对应目录**：

| 目录 | 平台 | 适用 |
|------|------|------|
| [`mac/`](./mac) | macOS | Mac 上的 Chrome |
| [`windows/`](./windows) | Windows 11 | Win11 上的 Chrome（已交叉编译验证可生成 exe） |

> ⚠️ 两个目录是**平行副本**：改一处要记得同步另一处（见下方「维护指南」）。

---

## 支持题型

| 题型 | 命令 | 说明 |
|------|------|------|
| 选择 / 填空题（整页内嵌题） | `go run .` | 自动识别选项字母 / 填空内容，自动提交，回读得分 |
| 程序片段编程题 | `go run . -mode=progap` | AI 补全代码中嵌的空，提交后读判题回显，未通过可重做 |

---

## 快速开始（以 macOS 为例）

```bash
cd mac
cp .env.example .env        # 填入火山引擎 ARK_API_KEY / ARK_MODEL_ID
go run .                   # 刷选择题/填空题；程序题用 go run . -mode=progap
```

Windows 步骤完全一致，只是进 `windows` 目录，且命令在 PowerShell 里用 `copy .env.example .env`。
两个目录里各自的 `README.md` 有该平台的完整环境安装说明（Go / Chrome / Python+ddddocr）。

---

## 项目结构

```
mac/  (或 windows/)
├── main.go              # CLI 入口（薄壳）：解析参数、交互式问答、打印结果
├── train/               # ★ 刷题流程本体（v1.3.0 从 main 包抽出来）
│   ├── api.go           #   对外接口：Run / Request / Result / 进度事件 / 哨兵错误
│   ├── train.go         #   登录 + 过 WAF + 选择/填空题 + 得分闭环
│   ├── progap.go        #   程序片段编程题 + 判题回显解析
│   └── preflight.go     #   启动浏览器前的占用闸门（端口 / profile 锁探测）
├── config/              # ★ 全部可调项的集中地
│   ├── config.go        #   环境变量读取 + Config + 重答策略判定
│   ├── site.go          #   站点 DOM 契约（选择器/XPath/正则/文案）+ 所有超时参数
│   └── env.go           #   平台差异唯一来源（Chrome 路径 / Python 探测 / profile 目录）
├── ai/                  # 大模型调用
│   ├── ai.go            #   统一入口：Answer / AnswerChoice / AnswerMulti / AnswerBlanks / AnswerCode
│   └── prompts.go       #   Prompt 集中管理与外部覆盖（prompts.json）
├── ocr/                 # ddddocr 验证码识别（Python 脚本封装）
├── server/              # 服务模式（可选）：HTTP + MySQL 队列 + worker + 进度页
├── tools/               # 排查用的小工具（cdpdiag / aitest / attachtest / logindump / wafprobe）
└── prompts.example.json # Prompt 模板，由 go run . -prompts-init 生成
```

> 各包都带 `_test.go`。`train` 之所以单独成包：Go **不允许导入 `package main`**，
> 服务端要复用刷题流程，流程就得先离开 `main` 包——「能跑起来的脚本」和
> 「能被别的代码调用的模块」是两回事。

**改一个东西该去哪里：**

| 想改什么 | 改哪里 |
|---------|-------|
| 站点改版了，选择器失效 | `config/site.go` |
| 嫌某步等太久 / 太快 | `config/site.go` 的 `Timeout*` / `Poll*` 常量 |
| 模型回答风格不对 | `prompts.json`（不用改代码） |
| 觉得重答太激进 / 太保守 | `.env` 的 `REANSWER_THRESHOLD` / `MAX_ANSWER_TRY` |
| 换 Python 或 Chrome 路径 | `.env` 或 `config/env.go` |

---

## 配置说明

两个版本都通过 `.env` 读配置（模板见 `.env.example`）：

```
ARK_API_KEY=你的火山引擎API-Key
ARK_MODEL_ID=你的模型接入点ID
```

其它可选环境变量（两平台通用，完整默认值见 `config/config.go`）：

| 变量 | 默认 | 作用 |
|------|------|------|
| `ASSIGN_KEYWORD` | `刷题` | 有多张作业卡时按标题关键词选卡 |
| `CHROME_PATH` | 自动探测 | 自定义浏览器可执行文件路径 |
| `CHROME_USER_DATA` | `.chrome-profile` | 浏览器 profile 目录，登录态存这里 |
| `CDP_PORT` | `9223` | 调试端口 |
| `CDP_URL` | 空 | 填了就接入一个已开着的浏览器，跳过自动登录 |
| `PYTHON_BIN` | 自动探测 | 自定义 Python 解释器（跑验证码识别用） |
| `REANSWER_THRESHOLD` | `0` | 得分 ≤ 此值即重答；`-1` = 从不重答 |
| `MAX_ANSWER_TRY` | `2` | 单题最多作答次数（含首次），最小 1 |
| `PROMPTS_FILE` | `prompts.json` | Prompt 覆盖文件路径 |
| `LOG_FORMAT` | `text` | 日志格式：`text` / `json` |

> **想要「和旧版完全一样」的行为**：设 `MAX_ANSWER_TRY=1` 或 `REANSWER_THRESHOLD=-1`，即只答一次不回读重试。

运行参数：

| 参数 | 作用 |
|------|------|
| `-mode=quiz` | 默认，整页选择/填空题 |
| `-mode=progap` | 程序片段编程题 |
| `-mode=progapdump` | 仅 dump 第一道程序题页面到 `progap.html`（调试） |
| `-dump` | quiz 模式下 dump 答题页到 `page.html`（调试） |
| `-log-json` | 日志改输出 JSON（等价于 `LOG_FORMAT=json`） |
| `-prompts-init` | 生成 `prompts.example.json` 并退出，不跑流程 |

### 调 Prompt 不用改代码

```bash
go run . -prompts-init      # 生成 prompts.example.json
mv prompts.example.json prompts.json
# 编辑 prompts.json，只写你想改的那几条，其余留空/不写即用内置默认
go run .
```

### 答错记录

次数用尽仍未得分的题会追加写到工作目录的 `wrong_answers.md`（题干 + 模型答案 + 得分），方便事后人工复盘。该文件已在 `.gitignore` 中排除。

---

## 服务模式（可选，v1.3.0 新增）

命令行用法**没有任何变化**。这一节是给「想让机器排队慢慢刷、多个人提交任务」的场景准备的。

为什么需要排队：一次刷题要独占一个 Chrome 进程、一个 profile、一个调试端口，
所以一台机器上并发不了。硬要并发，请求只会在浏览器那一层排长队——提交的人既看不到进度，
也不知道自己排在第几位。

```bash
# 1) 准备数据库（建表 + 建只给 DML 权限的应用账号）
mysql -uroot -p < server/schema.sql

# 2) 配置服务端环境（与 CLI 的 .env 分开，含密钥与数据库口令）
cat > server.env <<'EOF'
TASK_SECRET=至少32个字符的随机串，用 openssl rand -hex 32 生成
DB_DSN_USER=cqupt_app
DB_PASSWORD=改成你自己的口令
EOF
# 完整变量表见 server/README.md

# 3) 起服务
go run ./server
```

| 接口 | 作用 |
|------|------|
| `POST /api/tasks` | 提交任务，返回 `202 Accepted` + 任务 id |
| `GET /api/tasks/{id}` | 查状态与进度 |
| `GET /api/tasks/{id}/results` | 查逐题结果 |
| `DELETE /api/tasks/{id}` | 取消（只能取消还没开跑的） |
| `GET /` | 进度页（人看的） |
| `GET /healthz` | 健康检查 |

设计取舍、可靠性机制（重试退避 / 续租心跳 / 僵死回收 / 优雅退出排空）、
密码怎么存、有哪些已知限制，都写在 **[`server/README.md`](./mac/server/README.md)** 里。

> ⚠️ 进度页**没有鉴权**，服务默认只监听 `127.0.0.1`。要对外暴露必须先加认证，
> 否则任何人都能看到所有学号与错误信息。

---

## 维护指南（给日后更新的自己 / 协作者）

1. **改核心逻辑时，两个平台都要改**：`mac/` 和 `windows/` 下都有
   `main.go` `train/` `ai/` `config/` `ocr/` `server/` `tools/`。
   两处逻辑要保持一致；平台差异**只在** `config/env.go`（Chrome 路径候选、Python 候选、profile 目录）一处，
   改完记得两边都跑 `go test ./...`。
2. **改完记日志**：在 [CHANGELOG.md](./CHANGELOG.md) 加一条，并**升 `VERSION`**（SemVer：新功能升次版本号，修 Bug 升修订号；结构性重构也走次版本号）。
3. **两个目录的 README 顶部版本号**一并改成新的 vX.Y.Z（保持一致）。
4. **提交并打 tag**：
   ```bash
   git add -A
   git commit -m "v1.3.0: 服务模式 + train 包重构 + 占用闸门"
   git tag v1.3.0
   git push && git push --tags
   ```
5. **密钥永远不会进仓库**：`.env`（AI Key）与 `server.env`（`TASK_SECRET` + 数据库口令）都已被 `.gitignore` 排除；
   只提交 `mac/.env.example` / `windows/.env.example`。`prompts.json`（个人调参）与 `wrong_answers.md`（个人复盘）同样已排除。
6. **服务端测试默认跳过**：涉及 MySQL 的用例需要显式给 DSN 才会跑——
   ```bash
   CQUPT_TEST_DSN='cqupt_app:口令@tcp(127.0.0.1:3306)/cqupt_train?parseTime=true&loc=Local&charset=utf8mb4' go test ./...
   ```

---

## 常见问题

- **白屏 / 页面为空**：站点瑞数 WAF，脚本采用「原生启动 Chrome 过挑战再接管」的方式绕过，启动后请耐心等约 20 秒，不要去动弹出的浏览器窗口。
- **报错「建立浏览器控制连接失败」**：先跑 `go run ./tools/cdpdiag -headless`（不弹窗口），它会逐层报告卡在哪一步。
  最常见的一类成因是 **profile 或调试端口被另一个 Chrome 占着**——程序现在会在启动浏览器之前就把这种情况拦下来并直接报错，
  提示你退出所有 Chrome，或用 `CDP_PORT` / `CHROME_USER_DATA` 换一组端口与 profile。
- **验证码识别失败**：脚本自动重试 4 次，仍失败会停下来让你在浏览器里手动登录，登录后自动继续。
- **Windows 上连不上浏览器**：运行前先**关掉所有 Chrome 窗口**（Windows 已运行的 Chrome 会吞掉网址导致调试端口打不开）。
- **换更强的模型**：改 `.env` 里的 `ARK_MODEL_ID` 指向别的接入点即可（模型太弱可能得 0 分）。
- **某题反复重答还是 0 分**：说明模型确实做不出来（或题目有坑），会被记进 `wrong_answers.md`。可换更强模型，或调高 `REANSWER_THRESHOLD` 让它多试几次。
- **想省 token**：设 `MAX_ANSWER_TRY=1`，即只答一次、不回读重试。
