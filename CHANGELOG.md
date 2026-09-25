# 更新日志 (CHANGELOG)

本文件记录每个版本的主要变更。**新增功能 / 修复 Bug / 平台适配** 都在此登记，方便日后回溯。

版本号规则（语义化版本 SemVer）：`主版本.次版本.修订`
- 主版本：不兼容的结构性大改（如站点整体重构）
- 次版本：新增功能（如新增一类题型支持）
- 修订：修 Bug / 小调整

---

## v1.3.0（2026-09-25）

**服务化版本**。命令行用法与 v1.2.0 **完全一致**（`go run .` / `go run . -mode=progap` 行为不变），新增一条 HTTP 服务入口；另外修掉两个「**静默的错误答案**」类问题——它们都不报错，但结果是错的。

### 新增：服务模式（`server/`）

一次刷题要独占一个 Chrome、一个 profile、一个调试端口，所以一台机器上并发不了。要让多个人提交任务，只能排队。

- **入口**：`go run ./server`。CLI 与服务**共用 `config` / `ai` / `train` 三层**，只有入口不同。
- **队列**：直接用 MySQL 表兼队列（`tasks` 表 + `SELECT ... FOR UPDATE SKIP LOCKED`）。任务与排队状态在同一行、同一事务，天然没有「入队成功但业务回滚」的双写不一致；不需要额外引入 Redis / Kafka。
- **接口**：`POST /api/tasks` 提交（返回 `202 Accepted`）、`GET /api/tasks/{id}` 查状态、`GET /api/tasks/{id}/results` 查逐题结果、`DELETE /api/tasks/{id}` 取消、`GET /` 进度页、`GET /healthz` 健康检查。
- **进度可观测**：任务执行期间会落库逐条事件（登录成功 / 进入答题页 / 每题完成），进度页与 JSON 接口共用同一套视图转换。
- **可靠性六件套**：`available_at` 重试退避 · 续租心跳（`LOCK_TIMEOUT/3`，下限 10s）· 锁丢失即中止 · `RecoverStale` 回收僵死任务 · `FailExhausted` 截断死循环 · 优雅退出排空（收到停止信号只停止领新任务，手上的跑完）。
- **密码保护**：AES-256-GCM 认证加密后存 `password_enc`，明文只在一次函数调用内存在；`TASK_SECRET` 必填且不少于 32 字符，**不提供「先存明文」的降级路径**。
- 部署与运维细节见 [`mac/server/README.md`](./mac/server/README.md)（Windows 见 [`windows/server/README.md`](./windows/server/README.md)）。

### 结构调整：刷题流程抽成 `train` 包

- Go **不允许别的包导入 `package main`**，服务端要复用刷题流程，流程就必须先离开 `main` 包。
- 现在：`train/api.go`（对外接口 `Run` / `Request` / `Result` / 进度事件 / 哨兵错误）、`train/train.go`（登录 + 过 WAF + 选择填空题）、`train/progap.go`（程序题）、`train/preflight.go`（启动前的占用闸门）。
- 根 `main.go` 缩到 **176 行**，只剩 CLI 薄壳（解析参数、交互式问答、打印结果）。
- **「脚本」与「模块」的差别**：脚本把输入读自 stdin、结果打到终端、用退出码表示成败；模块必须把输入、输出、错误三样都变成显式的参数与返回值。这一步是服务化的第一道坎。

### 新增：浏览器连接诊断工具 `tools/cdpdiag`

把建连过程拆成四步逐层报告，并额外加一步「Chrome 进程还活着吗」——因为连接层报错常常只是**结果**，真正的问题是浏览器压根没活下来。加 `-headless` 可以不弹窗口地只查连接层。

### 修复

1. **启动浏览器不再静默接管别人的浏览器。**
   当 profile 或调试端口已被另一个 Chrome 占着时，新实例会把 URL 交给已有实例后自己退出，永远不会打印自己的 DevTools 地址。旧代码等满 15 秒后**静默退回按端口直连**，于是连上的是那个**已有实例**——而它打开的原生标签页此刻已经暴露在 CDP 之下，瑞数 WAF 绕过的前提（原生标签页全程无 CDP 会话）就此破裂。表现是页面白屏或 39 字节空壳这种**没有报错的失败**。
   现在：启动前先探测端口监听者与 profile 单实例锁，任意一条成立就**根本不启动 Chrome**，直接报错并说明处置办法；等满超时后重新收集证据再归因。该错误被标注为**不可重试**（服务端不再为一件注定失败的事反复起浏览器）。
2. **修 `TouchLock` 里一个把健康任务判成「锁丢失」的陷阱。**
   `RowsAffected` 返回的是**实际改变的行数**，不是匹配的行数。`locked_at` 是 `DATETIME(3)`，同一毫秒内连续两次续租时新旧值完全相等，MySQL 报 0 行——旧代码把它当「锁丢了」，于是 worker 会主动中止一个完全健康的任务，又是一次静默的错误答案。实测（固定会话时间）：
   ```
   第 1 次写入 NOW(3)        → Rows matched: 1  Changed: 1
   第 2 次写入同一个 NOW(3)  → Rows matched: 1  Changed: 0   ★
   第 3 次换成不同时刻        → Rows matched: 1  Changed: 1
   ```
   现在 0 行时会回查一次「锁到底在谁手里」再下结论。（心跳间隔下限 10 秒，正常配置下撞不进同一毫秒，所以线上极难触发；但依赖「间隔够大」是个隐含前提，不如把 0 行当成需要复核的信号。）
3. **配置文件读不了不再被静默跳过。**
   服务端启动时依次尝试 `server.env` / `.env.server` / `.env`，旧实现把 `godotenv.Load` 的**任何**错误都当成「换下一个候选试试」。
   于是只要 `server.env` 格式有问题，它就被整个跳过，用户看到的是「我明明写了 `TASK_SECRET`，程序却说必填」，而日志里一个字都不提文件有问题。
   最典型的触发方式在 Windows 上：PowerShell 5.1 的 `Out-File -Encoding utf8` 会写进 UTF-8 **BOM**，godotenv 解析首行时直接报
   `unexpected character "»" in variable name`（已实测确认）。
   现在区分「文件不存在」（跳过，正常）与「文件存在但读不了」（直接报错，并指出是哪个文件、最可能是什么原因）。

### 测试

- 测试文件从 3 个增至 **6 个，89 个测试函数 / 151 个用例，全部通过**（v1.2.0 为 99 个用例）：
  - 新增 `server/store_test.go`（39 个）：并发抢任务恰好一次、`EXPLAIN` 校验抢任务确实走 `idx_claim` 且无 filesort、重试退避、锁归属校验、`RecoverStale` 回收、`FailExhausted` 截断、`TouchLock` 丢锁识别、同毫秒续租回归、配置文件读取的三种情形、加密往返、配置校验、模板转义、中间件 504 兜底等。
  - 新增 `train/preflight_test.go`（10 个）：端口空闲 / CDP 端点 / 普通监听者三种形态，死锁不误报、活锁能识别、闸门放行与拦截。
  - `main_test.go` 的 JS/判题用例迁入 `train/train_test.go` 并扩充。
- **测试数据库隔离**：DB 测试默认跳过，设 `CQUPT_TEST_DSN` 才跑；每个测试用独有学号前缀，只清自己那几行，不清空整张表。
- **新增回归用例 `TestTouchLockSurvivesSameMillisecondRenewal`**：用会话级 `SET timestamp` 把 `NOW(3)` 钉死，**确定性地**复现同毫秒续租（回退修复后该用例必失败，已验证）。
- 验证：`go build ./...` / `go vet ./...` 干净，**151 PASS / 0 FAIL**，macOS / Windows(PE32+) / Linux(ELF) 三平台交叉编译均通过。

### 其它

- `.gitignore` 补充 `server.env` / `.env.server`（含 `TASK_SECRET` 与数据库口令）、`.chrome-profile-*/`（调试工具的 profile）。
- `config/site.go` 新增 `TimeoutPortProbe`（端口占用探测超时）。
- README 新增「报错「建立浏览器控制连接失败」怎么办」排查表。

---

## v1.2.0（2026-09-24）

**工程化重构版本**。功能行为默认不变（`MAX_ANSWER_TRY=1` / `REANSWER_THRESHOLD=-1` 时与 v1.1.1 完全等价），但内部结构大幅整理，并新增「得分闭环」能力。

### 新增功能

- **提交后回读分数闭环**：每题提交前后各读一次页面总分，差值即该题得分。
  - `REANSWER_THRESHOLD`（默认 `0`）：得分 ≤ 此值即重答；`-1` 表示永不重答（= v1.1.1 行为）。
  - `MAX_ANSWER_TRY`（默认 `2`，最小 `1`）：单题最多作答次数。
  - 选择/填空题在**同一页内**重填重交；程序题在**独立题目页**重新打开再作答。
- **答错题记录**：次数用尽仍低分的题追加写入 `wrong_answers.md`，含题干、模型答案、得分，方便人工复盘。
- **程序题判题结构化**：提交后读回显 iframe 原文，按关键词判定通过/失败。失败词优先于通过词（避免"未通过"含"通过"被误判）；不认识的中性文案按通过处理，不做无谓重答。
- **Prompt 外部覆盖**：`go run . -prompts-init` 生成 `prompts.example.json`；改名为 `prompts.json` 后只写想改的 key，其余保持内置默认（用 `map[string]string` 合并，避免未出现的字段被零值清空）。可用 `PROMPTS_FILE` 换路径。
- **结构化日志**：日志统一改用标准库 `slog`，支持 `-log-json` 参数或 `LOG_FORMAT=json` 切换为 JSON 输出（时间压缩为 `15:04:05`）。

### 结构调整

- **新增 `config` 包**——原先散落在各处的环境变量、DOM 选择器、XPath、正则、超时与轮询次数全部收拢：
  - `config/config.go`：`Config` 结构体 + 环境变量读取 + 重答策略判定。
  - `config/site.go`：站点 DOM 契约与时间参数的**唯一来源**（`Sel*` / `URL*` / `Text*` / `Re*` / `Timeout*` / `Poll*` / `Default*` 命名约定）。
  - `config/env.go`：**全部平台差异的唯一来源**（Chrome 路径候选、Python 候选、profile 目录），原先两份平台副本里各自硬编码的分支函数被删除。
- **新增 `ai/prompts.go`**——6 条 Prompt 集中管理（选择/多选/代码/程序题代码/程序题填空/重答提醒）。
- **`ai` 包接口统一**：`AnswerBlanks` / `AnswerCode` 取代原 `aiProgapBlanks` / `aiProgapCode`；所有能力都用 `extra ...string` 可变参数承载重答提醒，首次作答不传即行为不变。
- **代码量变化（实测）**：生产代码 1691 行（`main.go` 933 + `progap.go` 621 + `ai/ai.go` 137）→ 2535 行（7 个文件），另加测试代码 738 行。
  单看 `main.go` 是 933 → 1064 行、`progap.go` 是 621 → 660 行：常量外迁让文件变短，但新增的得分闭环功能又让它变长，净增。
  **所以 v1.2.0 不是"代码变少"，是"代码变多但可维护性换了个量级"**——多的部分主要买了得分闭环、可测性和配置单一来源。
- **模块名修正**：`module main` → `module cqupt`。原先禁止测试包导入 `package main` 所在模块，导致根包根本无法写单测，也无法 `go install`。

### 测试

- **新增 3 个测试文件，共 99 个用例**（修复前为 0）：
  - `config/config_test.go`：重答策略边界（含阈值 `-1`、次数用尽）、环境变量回退、默认值、选择器非空、登录 URL 参数。
  - `ai/ai_test.go`：`CleanCode` / `SplitNonEmptyLines` / `firstUpperLetter` 表驱动用例、Prompt 默认值完整性、格式化动词数量校验、覆盖文件的缺失/部分/空值/非法 JSON 四种容错、模板往返。
  - `main_test.go`：JS 拼接的参数转义（含双引号/换行/反斜杠）、JS 常量确实引用共享选择器（8 项，防有人写回硬编码）、判题 13 例、`truncate` 按 rune 切不产生乱码。
- **Bug 修复**：`truncate` 原为 `s[:n]` 按**字节**截断，中文会切出乱码，改为 `[]rune(s)[:n]`。

### 其它

- `.gitignore` 补充 `prompts.json`（个人调参）、`wrong_answers.md`（个人复盘）、`/cqupt`、`/cqupt-*`、`*.dll`。
- 依赖升级以适配新版 Go 工具链：`bytedance/sonic` v1.14.1 → v1.15.4（连带 `sonic/loader` v0.3.0 → v0.5.2）、`go-json-experiment/json` 升到 `v0.0.0-20260820222146-...`，`go.mod` 的 `go` 指令 `1.25.1` → `1.26`。
- 验证：`go build ./...` / `go vet ./...` 干净，`go test ./...` **99 PASS / 0 FAIL**，macOS / Windows(PE32+) / Linux(ELF) 三平台交叉编译均通过。

---

## v1.1.1（2026-09-23）

- **文档**：README 增加「风险提示与免责声明」（仅供学习交流、**严禁盈利**、风险自负）、「求 Star」引导；各平台 README 顶部同步免责声明与版本号。
- 项目命名统一用 `cpp` 表示 C++ 语言（不再用 `c`）。

## v1.1.0（2026-09-23）

- **新增程序片段编程题支持**（`-mode=progap`）：自动枚举作业页程序题、提取题干、AI 补全代码中嵌的空、自动提交并回读判题结果。
- **跨平台双版本**：本仓库分为 `mac/`（macOS）与 `windows/`（Windows 11 + Chrome）两个子目录，核心逻辑一致，仅浏览器路径 / Python 命令解析按平台适配。
- **程序题自动 dump 调试**：`-mode=progapdump` 只 dump 第一道程序题页面到 `progap.html`，便于核对选择器。
- **README 增加版本号与维护指南**。

## v1.0.0（2026-09-22）

- 绕过瑞数 WAF（原生启动 Chrome 过挑战后再接管 CDP）。
- OCR 自动识别验证码（ddddocr），失败自动重试 4 次并支持人工兜底。
- 登录后自动进入作业卡片（默认选标题含「刷题」的卡，可用 `ASSIGN_KEYWORD` 覆盖）。
- 整页选择/填空题自动作答（单选 `AnswerChoice`、多空 `AnswerMulti`、自动提交 + 校验「已提交」）。

---

> 维护提示：每次改完代码，记得同时更新 `mac/` 与 `windows/` 两处的相同逻辑、在这里加一条记录、并把 `VERSION` 升一位，最后打 git tag（如 `v1.1.0`）。
