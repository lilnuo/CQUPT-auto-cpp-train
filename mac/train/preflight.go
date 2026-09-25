package train

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"cqupt/config"
)

// 本文件只干一件事：**在启动浏览器之前确认这台机器上没有另一个 Chrome
// 正占着我们唯一需要的那点资源**，以及在"没读到调试地址"时给出真实归因。
//
// 为什么值得单独成文件：这里治的是一个实测出来的坏行为。
// profile 或调试端口被另一个 Chrome 占着时，新起的实例会把 URL 交给已有实例
// 然后自己退出，永远不会打印自己的 DevTools 地址；旧代码等满
// TimeoutChromeWSWait 后**静默退回按端口直连**。退回之后连上的是那个
// **已有实例**，而它打开的原生标签页此刻已经暴露在 CDP 之下——
// 瑞数 WAF 绕过的前提（原生标签页全程无 CDP 会话）就此破裂，
// 表现是页面白屏或 39 字节空壳，而不是一条能看懂的报错。
//
// 现在的原则：**宁可明确失败，也不接管一个来路不明的浏览器**。

// cdpVersionPath 是 CDP 的版本端点，任何开着调试端口的 Chrome 都会响应它。
const cdpVersionPath = "/json/version"

// portProbe 是一次端口探测的结果。
type portProbe struct {
	InUse bool // 有进程在这个端口上监听
	IsCDP bool // 监听者确实是 Chrome 的 CDP 端点，而不只是恰好占了这个端口
}

// probeCDPPort 探测 127.0.0.1:port 上有没有东西在监听，并尽量判断是不是 CDP 端点。
//
// 用裸 net.Dial 而不是 http.Client：不经任何代理设置，行为最确定，
// 不会因为环境里配了 HTTP_PROXY 而把本机请求拐到别处去。
// 连不上就是"端口空闲"的正常回答，不是错误路径。
func probeCDPPort(port string) portProbe {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", port), config.TimeoutPortProbe)
	if err != nil {
		return portProbe{}
	}
	defer conn.Close()

	// 只发一个最小 HTTP 请求、只做子串判断：我们的问题"它认不认这条路"，
	// 一个完整的 HTTP 客户端在这里属于多余依赖。
	_ = conn.SetDeadline(time.Now().Add(config.TimeoutPortProbe))
	_, _ = fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n", cdpVersionPath)
	body, _ := io.ReadAll(io.LimitReader(conn, 8*1024))
	return portProbe{InUse: true, IsCDP: bytes.Contains(body, []byte("webSocketDebuggerUrl"))}
}

// profileLockHolder 报告 profile 目录的单实例锁被哪个进程持有，空串表示"没有活的持有者"。
//
// Chrome 会在 user-data-dir 下放一个 SingletonLock，指向最后一个使用该 profile
// 的实例，内容形如 "<主机名>-<pid>"。
//
// **只看锁文件在不在是不够的**：本流程自己就是用 Kill（SIGKILL）收尾的，
// 每次运行都会留下一个死锁，而 Chrome 遇到死锁会自行接管、照常启动。
// 所以必须再确认那个 PID 还活着，否则第二次运行就会误报"profile 被占用"。
//
// 返回空串的三种情形都属于"拿不准就不说"：锁不存在、锁指向的进程已死、
// 或当前平台不支持探测进程存活（Windows 上 os.Process.Signal 会返回"不支持"）。
// 宁可少说一句，也不要报一个假的占用。
func profileLockHolder(profile string) string {
	target, err := os.Readlink(filepath.Join(profile, "SingletonLock"))
	if err != nil {
		return ""
	}
	// PID 在最后一个 "-" 之后；主机名里本身可能带 "-"，所以从右往左找。
	idx := strings.LastIndex(target, "-")
	if idx <= 0 || idx == len(target)-1 {
		return ""
	}
	pid, err := strconv.Atoi(target[idx+1:])
	if err != nil || pid <= 0 {
		return ""
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return ""
	}
	// signal 0 不真的发信号，只做"进程存在且我有权限"的检查。
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		return ""
	}
	return fmt.Sprintf("PID %d（锁 %s）", pid, target)
}

// browserBusyError 把探测到的证据拼成一条可执行的错误：先说结论，再列证据，最后给处置办法。
func browserBusyError(port, profile string, probe portProbe, holder string) error {
	var b strings.Builder
	b.WriteString("不接管来路不明的浏览器。证据：")
	var seen []string
	if probe.IsCDP {
		seen = append(seen, fmt.Sprintf("调试端口 %s 上已有 Chrome 的 CDP 端点在响应", port))
	} else if probe.InUse {
		seen = append(seen, fmt.Sprintf("调试端口 %s 已被其它进程占用（但不是 CDP 端点）", port))
	}
	if holder != "" {
		seen = append(seen, fmt.Sprintf("profile %s 正被 %s 持有", profile, holder))
	}
	b.WriteString(strings.Join(seen, "；"))
	b.WriteString("。处置：退出所有 Chrome（或结束上面这些进程），" +
		"或用 CDP_PORT / CHROME_USER_DATA 换一组端口与 profile。" +
		"诊断命令：go run ./tools/cdpdiag -headless")
	return fmt.Errorf("%w：%s", ErrBrowserBusy, b.String())
}

// guardBrowserStartup 在启动 Chrome 之前做占用检查，被占用时直接返回错误。
//
// 提前拦住的好处有两个：一是根本不白开一次窗口（用户屏幕上不会一闪而过），
// 二是避免"退回按端口直连"把别人的浏览器接管过来——那条路会把 WAF 绕过的
// 前提破坏掉，而且是静默的。
func guardBrowserStartup(port, profile string) error {
	probe := probeCDPPort(port)
	holder := profileLockHolder(profile)
	if !probe.InUse && holder == "" {
		return nil
	}
	return browserBusyError(port, profile, probe, holder)
}
