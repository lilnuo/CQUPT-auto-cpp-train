# 重庆邮电大学程序设计平台自动刷题脚本

> 适配站点：https://prg.cqupt.edu.cn （含瑞数 WAF 防护，脚本已自动绕过）

一个能**自动登录 + 自动做题**的脚本：启动浏览器 → 过 WAF → OCR 识别验证码登录 → 选作业卡 → 逐题 AI 作答 → 自动提交。

当前版本：**v1.1.0**（见 [VERSION](./VERSION) / [CHANGELOG.md](./CHANGELOG.md)）

---

## 平台版本对照

本仓库同时收录两个平台版本，核心逻辑一致，仅浏览器路径 / Python 命令解析按系统适配。**请按你的系统进对应目录**：

| 目录 | 平台 | 适用 |
|------|------|------|
| [`mac/`](./mac) | macOS | Mac 上的 Chrome |
| [`windows/`](./windows) | Windows 11 | Win11 上的 Chrome（已交叉编译验证可生成 exe） |

> ⚠️ 两个目录是**平行副本**：改一处要记得同步另一处（见下方「维护指南」）。

---

## 支持题型

| 题型 | 命令 | 说明 |
|------|------|------|
| 选择 / 填空题（整页内嵌题） | `go run .` | 自动识别选项字母 / 填空内容，自动提交 |
| 程序片段编程题 | `go run . -mode=progap` | AI 补全代码中嵌的空，自动提交并回读判题结果 |

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

## 配置说明

两个版本都通过 `.env` 读配置（模板见 `.env.example`）：

```
ARK_API_KEY=你的火山引擎API-Key
ARK_MODEL_ID=你的模型接入点ID
```

其它可选环境变量（两平台通用）：

| 变量 | 作用 |
|------|------|
| `ASSIGN_KEYWORD` | 有多张作业卡时按标题关键词选卡（默认「刷题」） |
| `CHROME_PATH` | 自定义浏览器可执行文件路径 |
| `PYTHON_BIN` | 自定义 Python 解释器（跑验证码识别用） |
| `CDP_PORT` | 调试端口（默认 9223） |

运行参数：

| 参数 | 作用 |
|------|------|
| `-mode=quiz` | 默认，整页选择/填空题 |
| `-mode=progap` | 程序片段编程题 |
| `-mode=progapdump` | 仅 dump 第一道程序题页面到 `progap.html`（调试） |
| `-dump` | quiz 模式下 dump 答题页到 `page.html`（调试） |

---

## 维护指南（给日后更新的自己 / 协作者）

1. **改核心逻辑时，两个平台都要改**：`mac/` 和 `windows/` 下都有 `main.go` `progap.go` `ai/` `ocr/`。
   两处逻辑要保持一致；平台差异只在 `findChrome()`（浏览器路径）和 `findPython()`（python 命令名）两处，见各目录 `main.go`。
2. **改完记日志**：在 [CHANGELOG.md](./CHANGELOG.md) 加一条，并**升 `VERSION`**（SemVer：新功能升次版本号，修 Bug 升修订号）。
3. **两个目录的 README 顶部版本号**一并改成新的 vX.Y.Z（保持一致）。
4. **提交并打 tag**：
   ```bash
   git add -A
   git commit -m "v1.1.0: 程序题支持 + 跨平台"
   git tag v1.1.0
   git push && git push --tags
   ```
5. **密钥永远不会进仓库**：`.env` 已被 `.gitignore` 排除；只提交 `.env.example`。

---

## 常见问题

- **白屏 / 页面为空**：站点瑞数 WAF，脚本采用「原生启动 Chrome 过挑战再接管」的方式绕过，启动后请耐心等约 20 秒，不要去动弹出的浏览器窗口。
- **验证码识别失败**：脚本自动重试 4 次，仍失败会停下来让你在浏览器里手动登录，登录后自动继续。
- **Windows 上连不上浏览器**：运行前先**关掉所有 Chrome 窗口**（Windows 已运行的 Chrome 会吞掉网址导致调试端口打不开）。
- **换更强的模型**：改 `.env` 里的 `ARK_MODEL_ID` 指向别的接入点即可（模型太弱可能得 0 分）。
