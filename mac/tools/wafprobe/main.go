// wafprobe 探针：按 runTrainer 相同方式原生启动 Chrome 打开登录页，
// 然后每 3 秒打印所有标签页（type/url/title），持续 90 秒，
// 用于观察瑞数 WAF 挑战的真实过程（URL 是否变化、title 何时出现）。
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"time"
)

const (
	loginURL = "https://prg.cqupt.edu.cn/indexcs/simple.jsp?loginErr=0"
	port     = "9224"
	profile  = ".chrome-profile-wafprobe"
)

type tab struct {
	ID    string `json:"id"`
	Type  string `json:"type"`
	URL   string `json:"url"`
	Title string `json:"title"`
}

func main() {
	chromeBin := "/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"
	if _, err := os.Stat(chromeBin); err != nil {
		fmt.Println("!! 未找到 Chrome:", err)
		os.Exit(1)
	}
	cmd := exec.Command(chromeBin,
		"--remote-debugging-port="+port,
		"--user-data-dir="+profile,
		"--no-first-run",
		"--no-default-browser-check",
		loginURL,
	)
	if err := cmd.Start(); err != nil {
		fmt.Println("!! 启动失败:", err)
		os.Exit(1)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	fmt.Println(">> Chrome 已启动，开始观察...")

	for i := 0; i < 30; i++ {
		time.Sleep(3 * time.Second)
		resp, err := http.Get("http://127.0.0.1:" + port + "/json/list")
		if err != nil {
			fmt.Printf("[t=%2ds] 端口未就绪: %v\n", (i+1)*3, err)
			continue
		}
		var tabs []tab
		if err := json.NewDecoder(resp.Body).Decode(&tabs); err != nil {
			fmt.Printf("[t=%2ds] 解析失败: %v\n", (i+1)*3, err)
			resp.Body.Close()
			continue
		}
		resp.Body.Close()
		fmt.Printf("[t=%2ds] %d 个 target:\n", (i+1)*3, len(tabs))
		for _, t := range tabs {
			url := t.URL
			if len(url) > 80 {
				url = url[:80] + "..."
			}
			fmt.Printf("    type=%-10s title=%q url=%s\n", t.Type, t.Title, url)
		}
	}
}
