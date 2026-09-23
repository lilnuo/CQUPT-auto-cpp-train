# 重庆邮电大学程序设计平台自动刷题脚本

> **版本：v1.1.0（2026-09-23）** ｜ **平台：macOS** ｜ 更新日志见根目录 [CHANGELOG.md](../CHANGELOG.md)

你是否也被学校无用的 C++ 语言刷题所困扰？此脚本可以帮助你解决烦恼。

适配站点：https://prg.cqupt.edu.cn （含瑞数 WAF 防护，脚本已自动绕过）

## 功能

| 题型 | 命令 | 说明 |
|------|------|------|
| 选择/填空题（整页内嵌题） | `go run .` | 自动识别选项字母/填空内容，自动提交 |
| 程序片段编程题 | `go run . -mode=progap` | AI 补全代码中嵌的空，自动提交并回读判题结果 |

全流程自动：启动浏览器 → 过 WAF 挑战 → 识别验证码登录（ddddocr）→ 选择作业卡 → 逐题作答 → 自动提交。中断后重跑会跳过已提交的题。

## 前置条件

1. 安装 Go 和 Chrome 浏览器
2. 在 [火山引擎](https://console.volcengine.com/ark) 开通服务领取免费额度，创建 API Key 和模型接入点
3. 验证码识别依赖 ddddocr（Python）：
   ```bash
   pip install ddddocr
   ```
   脚本会依次尝试 `PYTHON_BIN` 环境变量 → 本机 venv → 系统 `python3` 来调用它。
   你也可以在 `ai` 包里重写 `InitAI` 换成别家模型（模型太弱可能得 0 分）。

## 使用方法

```bash
# 1. 配置凭据：复制模板并填入你自己的 Key
cp .env.example .env
# 然后编辑 .env，填入 ARK_API_KEY 和 ARK_MODEL_ID

# 2. 刷选择/填空题
go run .

# 3. 刷程序片段编程题
go run . -mode=progap
```

按提示输入学号、密码、想刷的题目数量（建议先填 1~3 验证，确认没问题再放量）。

## 可选参数 / 环境变量

| 项 | 作用 |
|----|------|
| `-mode=quiz` | 默认，整页选择填空题 |
| `-mode=progap` | 程序片段编程题 |
| `-mode=progapdump` | 只 dump 第一道程序题页面到 progap.html（调试用） |
| `-dump` | quiz 模式下 dump 答题页到 page.html（调试用） |
| `ASSIGN_KEYWORD` | 有多张作业卡时按标题关键词选卡（默认"刷题"） |
| `CHROME_PATH` | 自定义 Chrome 路径 |
| `CDP_PORT` | 调试端口（默认 9223） |

## 常见问题

- **验证码识别失败**：脚本会自动重试 4 次，仍失败会停下来让你在弹出的浏览器里手动登录，登录后自动继续。
- **白屏/页面为空**：站点有瑞数 WAF，脚本采用"原生启动 Chrome 过挑战后再接管"的方式绕过，启动后请耐心等待约 20 秒，不要去动弹出的浏览器窗口。
- **想换作业**：设 `ASSIGN_KEYWORD=作业标题里的关键词`。

## 效果图

![show](img/show.png)
