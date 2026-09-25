package config

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// ============================================================================
// 本文件是**全部平台差异的唯一来源**。
//
// 改造前，mac/ 与 windows/ 是两份平行副本，差异散落在各自的 main.go 里
// （findChrome / findPython 两处），改一处忘另一处就会长期不一致。
// 现在差异全部收敛到这里，两个平台共用同一份业务代码。
// ============================================================================

// ChromeCandidates 返回按优先级排列的浏览器可执行文件候选路径。
// 不同平台的默认安装位置不同，故按 GOOS 分支。
func ChromeCandidates() []string {
	switch runtime.GOOS {
	case "darwin":
		return []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
			"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge",
		}
	case "windows":
		var out []string
		// 系统级安装
		if pf := os.Getenv("ProgramFiles"); pf != "" {
			out = append(out, filepath.Join(pf, "Google", "Chrome", "Application", "chrome.exe"))
		}
		if pf := os.Getenv("ProgramFiles(x86)"); pf != "" {
			out = append(out, filepath.Join(pf, "Google", "Chrome", "Application", "chrome.exe"))
		}
		// 用户级安装（仅装给自己时走这条，很常见）
		if la := os.Getenv("LOCALAPPDATA"); la != "" {
			out = append(out,
				filepath.Join(la, "Google", "Chrome", "Application", "chrome.exe"),
				filepath.Join(la, "Microsoft", "Edge", "Application", "msedge.exe"),
			)
		}
		// 兜底：写死常见路径（环境变量缺失时用）
		out = append(out,
			`C:\Program Files\Google\Chrome\Application\chrome.exe`,
			`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
		)
		return out
	default: // linux 及其他
		return []string{
			"/usr/bin/google-chrome",
			"/usr/bin/google-chrome-stable",
			"/usr/bin/chromium",
			"/usr/bin/chromium-browser",
		}
	}
}

// ChromePathNames 返回可交给 PATH 查找的浏览器命令名。
func ChromePathNames() []string {
	switch runtime.GOOS {
	case "darwin":
		return []string{"google-chrome", "chromium"}
	case "windows":
		return []string{"chrome", "msedge"}
	default:
		return []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser"}
	}
}

// FindChrome 定位浏览器可执行文件。
// 优先级：CHROME_PATH 环境变量 > 各平台默认安装路径 > PATH 查找。
func FindChrome() (string, error) {
	if p := C.ChromePath; p != "" {
		return p, nil
	}
	for _, p := range ChromeCandidates() {
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	for _, name := range ChromePathNames() {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("未找到 Chrome/Chromium/Edge 浏览器，请安装或用 CHROME_PATH 指定可执行文件路径")
}

// PythonCandidates 返回按优先级排列的 Python 解释器候选。
//
// 顺序说明：优先用本机隔离 venv（ddddocr 通常装在隔离环境里，
// 不动用户的全局 Python）；其次回退到系统命令名，兼容各平台差异
// （Windows 上 python 通常可用，macOS/Linux 上是 python3）。
func PythonCandidates() []string {
	if C.PythonBin != "" {
		return []string{C.PythonBin}
	}
	var out []string
	if home, err := os.UserHomeDir(); err == nil {
		out = append(out, filepath.Join(home, ".workbuddy", "binaries", "python", "envs", "default", "bin", "python"))
	}
	if runtime.GOOS == "windows" {
		out = append(out, "python", "py", "python3")
	} else {
		out = append(out, "python3", "python")
	}
	return out
}

// FindPython 返回第一个真正可用的 Python 解释器。
//
// 改造前这里直接把 "python3" 交给 exec，解释器不存在时报的是
// "exec: python3: executable file not found" 这类底层错误，不好排查；
// 现在逐个探测，全部失败时给出明确指引。
func FindPython() (string, error) {
	var tried []string
	for _, p := range PythonCandidates() {
		tried = append(tried, p)
		if filepath.IsAbs(p) {
			if _, err := os.Stat(p); err == nil {
				return p, nil
			}
			continue
		}
		if abs, err := exec.LookPath(p); err == nil {
			return abs, nil
		}
	}
	return "", fmt.Errorf("未找到可用的 Python 解释器（尝试过 %v），请安装 Python 并用 PYTHON_BIN 指定路径", tried)
}

// UserDataDir 返回浏览器 profile 目录（登录态持久化于此）。
func UserDataDir() string {
	if C.ChromeUserData != "" {
		return C.ChromeUserData
	}
	return DefaultChromeProfile
}
