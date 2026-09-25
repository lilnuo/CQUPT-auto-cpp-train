# 更新日志 (CHANGELOG)

本文件记录每个版本的主要变更。**新增功能 / 修复 Bug / 平台适配** 都在此登记，方便日后回溯。

版本号规则（语义化版本 SemVer）：`主版本.次版本.修订`
- 主版本：不兼容的结构性大改（如站点整体重构）
- 次版本：新增功能（如新增一类题型支持）
- 修订：修 Bug / 小调整

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
