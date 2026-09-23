# 重庆邮电大学程序设计平台自动刷题脚本（Windows 11 + Chrome 版）

> **版本：v1.1.1（2026-09-23）** ｜ **平台：Windows 11** ｜ 更新日志见根目录 [CHANGELOG.md](../CHANGELOG.md)

> ⚠️ **仅限个人学习交流，严禁用于任何盈利目的**；使用风险自负（详见根目录 [README](../README.md) 的免责声明）。
> ⭐ 觉得好用的话，欢迎给仓库点个 **Star**。

> macOS 版在另一个文件夹，这份是**专门适配 Windows 11 + Google Chrome** 的版本。

适配站点：https://prg.cqupt.edu.cn （站点有瑞数 WAF 防护，脚本已自动绕过）

## 一、环境准备（Windows 11）

### 1. 安装 Go
到 https://go.dev/dl/ 下载 Windows 版 `go*.msi`，一路下一步安装。装完打开 **PowerShell** 验证：
```powershell
go version
```
看到版本号就 OK。

### 2. 安装 Google Chrome
https://www.google.com/chrome/ 下载安装即可。脚本会自动在以下位置找 `chrome.exe`：
- `%LOCALAPPDATA%\Google\Chrome\Application\chrome.exe`（**无管理员安装时的默认位置**）
- `C:\Program Files\Google\Chrome\Application\chrome.exe`
- `C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`
- 找不到就试 Edge，或用环境变量 `CHROME_PATH` 手动指定

### 3. 安装 Python + 验证码识别库
到 https://www.python.org/downloads/windows/ 下载安装 **Python 3.x**（安装时务必勾选 **Add Python to PATH**）。装完后在 PowerShell 里装识别库：
```powershell
pip install ddddocr
```
脚本会自动探测 `python` / `py` / `python3` 命令。想手动指定解释器就设环境变量 `PYTHON_BIN`。

### 4. 配置 AI 凭据（火山引擎）
到 https://console.volcengine.com/ark 注册开通（有免费额度），创建 **API Key** 和**模型接入点（Model ID）**。

然后把模板复制成正式配置并填入：
```powershell
copy .env.example .env
```
用记事本打开 `.env`，填成这样：
```
ARK_API_KEY=你的API-Key
ARK_MODEL_ID=你的模型ID
```

## 二、运行

在**项目文件夹**里打开 PowerShell（资源管理器地址栏输入 `powershell` 回车即可），然后：

```powershell
# 刷选择/填空题
go run .

# 刷程序片段编程题
go run . -mode=progap
```

按提示依次输入 **学号 → 密码 → 想刷的题目数量**（建议先填 1~3 验证，确认没问题再放量）。

## 三、⚠️ Windows 上的重要注意事项

1. **运行前请先关闭所有已打开的 Chrome 窗口**。Windows 上如果 Chrome 已经在跑，新启动的进程可能把网址丢给已有实例，导致调试端口没开、脚本连不上浏览器。
2. 启动后会有约 **20 秒静默等待**（在过 WAF 挑战），期间不要去点弹出的浏览器窗口。
3. 验证码识别会自动重试 4 次；万一识别不过，脚本会停住让你在弹出来的浏览器里**手动登录**，登录后自动继续。
4. 首次运行会生成 `.chrome-profile` 文件夹保存登录态，之后重跑通常不用再输验证码。
5. 如果浏览器一直连不上，用环境变量指定路径：
   ```powershell
   $env:CHROME_PATH = "C:\Users\你的用户名\AppData\Local\Google\Chrome\Application\chrome.exe"
   go run .
   ```

## 四、参数一览

| 项 | 作用 |
|----|------|
| `-mode=quiz` | 默认，整页选择/填空题 |
| `-mode=progap` | 程序片段编程题 |
| `-mode=progapdump` | 只 dump 第一道程序题页面到 progap.html（调试用） |
| `-dump` | quiz 模式下 dump 答题页到 page.html（调试用） |
| `ASSIGN_KEYWORD` | 有多张作业卡时按标题关键词选卡（默认"刷题"），例如 `$env:ASSIGN_KEYWORD="平时"` |
| `CHROME_PATH` | 自定义 chrome.exe 路径 |
| `PYTHON_BIN` | 自定义 python 解释器路径 |
| `CDP_PORT` | 调试端口（默认 9223） |

## 五、常见问题

| 现象 | 原因 / 处理 |
|------|------------|
| `未找到 Chrome/Edge 浏览器` | 用 `CHROME_PATH` 指定 chrome.exe |
| `OCR 执行失败` | `pip install ddddocr` 没装好，或 Python 没加入 PATH；设 `PYTHON_BIN` 指定 |
| 页面空白 / 一直连不上 | 先关掉所有 Chrome 窗口再跑 |
| `ai初始化失败` | `.env` 没建或 Key 填错（注意别用 `copy` 出来的模板原名 `.env.example`） |
| 某题得分 0 | 模型能力问题，可换更强的模型接入点 |

## 效果图

![show](img/show.png)
