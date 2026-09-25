package train

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// ============================================================================
// 启动前的占用检查
//
// 这一组测试守的是一条行为红线：**宁可明确失败，也不接管别人的浏览器**。
// 旧行为是等满超时后静默退回按端口直连，退回之后连上的是占位者的 Chrome，
// 而它的原生标签页此刻已经暴露在 CDP 下——瑞数 WAF 绕过的前提就此破裂，
// 表现是白屏而不是报错。这种"静默降级"用肉眼测很难发现，所以钉在测试里。
// ============================================================================

// freePort 返回一个当前空闲的端口号（拿到就立刻释放，存在极小的竞态，测试够用）。
func freePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("取空闲端口失败: %v", err)
	}
	defer ln.Close()
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatalf("解析监听地址失败: %v", err)
	}
	return port
}

// occupyPort 在指定端口上起一个监听者，返回清理函数。
// handler 为 nil 时使用默认（只监听、不回包）。
func occupyPort(t *testing.T, port string, handler http.Handler) func() {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:"+port)
	if err != nil {
		t.Fatalf("占用端口 %s 失败: %v", port, err)
	}
	if handler == nil {
		// 不接受也不回包：用来测"有人在监听，但不是 CDP 端点"。
		// 探测方会等到自己的超时才返回，这正是我们想验证的形态。
		return func() { _ = ln.Close() }
	}
	srv := &http.Server{Handler: handler}
	go func() { _ = srv.Serve(ln) }()
	return func() { _ = srv.Close() }
}

func TestProbeCDPPortReportsFreePort(t *testing.T) {
	port := freePort(t)
	p := probeCDPPort(port)
	if p.InUse {
		t.Errorf("空闲端口 %s 被报成已占用", port)
	}
	if p.IsCDP {
		t.Errorf("空闲端口 %s 被报成 CDP 端点", port)
	}
}

func TestProbeCDPPortDetectsCDPEndpoint(t *testing.T) {
	port := freePort(t)
	// 冒烟一个最小 CDP：只要 /json/version 回带 webSocketDebuggerUrl 的 JSON 即可。
	h := http.NewServeMux()
	h.HandleFunc("/json/version", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"Browser":"Chrome/140","webSocketDebuggerUrl":"ws://127.0.0.1:1/devtools/browser/x"}`)
	})
	cleanup := occupyPort(t, port, h)
	defer cleanup()

	p := probeCDPPort(port)
	if !p.InUse {
		t.Fatalf("端口 %s 上有监听者，却没被探测到", port)
	}
	if !p.IsCDP {
		t.Errorf("端口 %s 上是 CDP 端点，却判成普通占用（回包含 webSocketDebuggerUrl 却没识别出来）", port)
	}
}

func TestProbeCDPPortDetectsPlainListener(t *testing.T) {
	port := freePort(t)
	// 一个不认 HTTP 的监听者：要验证的是"有人在监听"这件事本身就能被抓到，
	// 因为那种情况下我们的 Chrome 也照样绑不上这个端口。
	cleanup := occupyPort(t, port, nil)
	defer cleanup()

	p := probeCDPPort(port)
	if !p.InUse {
		t.Fatalf("端口 %s 上有监听者，却没被探测到", port)
	}
	if p.IsCDP {
		t.Errorf("端口 %s 上不是 CDP 端点，却被判成 CDP", port)
	}
}

// ---- profile 单实例锁 ----

func TestProfileLockHolderIgnoresStaleLock(t *testing.T) {
	dir := t.TempDir()
	// 指向一个几乎不可能存在的 PID。真实场景对应的就是"上一次被强杀留下的死锁"——
	// 本流程自己就是用 Kill 收尾的，这种锁每次都会留下，绝不能因此误报占用。
	writeLock(t, dir, "somehost-999999")
	if got := profileLockHolder(dir); got != "" {
		t.Errorf("死锁（持有者进程已不存在）应返回空串，得到 %q", got)
	}
}

func TestProfileLockHolderDetectsLiveHolder(t *testing.T) {
	if runtime.GOOS == "windows" {
		// os.Process.Signal 在 Windows 上不支持 signal 0，此函数会安静地返回空串，
		// 这是有意的降级（宁可不报，也不报假的占用）。
		t.Skip("Windows 上进程存活探测不可用，属于已知降级")
	}
	dir := t.TempDir()
	writeLock(t, dir, fmt.Sprintf("somehost-%d", os.Getpid()))
	if got := profileLockHolder(dir); got == "" {
		t.Error("锁的持有者（本进程）活着，却报告没有持有者")
	}
}

func TestProfileLockHolderHandlesMissingOrMalformedLock(t *testing.T) {
	dir := t.TempDir()
	if got := profileLockHolder(dir); got != "" {
		t.Errorf("没有锁文件时应返回空串，得到 %q", got)
	}
	writeLock(t, dir, "没有pid的锁")
	if got := profileLockHolder(dir); got != "" {
		t.Errorf("格式不认识的锁应返回空串（宁可少说），得到 %q", got)
	}
}

// writeLock 在 dir 下造一个 SingletonLock 符号链接，内容为 target。
// 建不了符号链接的平台（Windows 权限）直接跳过本用例。
func writeLock(t *testing.T, dir, target string) {
	t.Helper()
	link := filepath.Join(dir, "SingletonLock")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("本环境无法创建符号链接，跳过：%v", err)
	}
}

// ---- 启动闸门 ----

func TestGuardBrowserStartupPassesWhenFree(t *testing.T) {
	// profile 用临时空目录：不存在的锁、没人监听的端口 → 放行
	if err := guardBrowserStartup(freePort(t), t.TempDir()); err != nil {
		t.Errorf("端口空闲、profile 无锁时应放行，却报错：%v", err)
	}
}

func TestGuardBrowserStartupRejectsBusyPort(t *testing.T) {
	port := freePort(t)
	cleanup := occupyPort(t, port, nil)
	defer cleanup()

	err := guardBrowserStartup(port, t.TempDir())
	if err == nil {
		t.Fatal("端口已被占用却没报错——这等于又回到了静默接管别人浏览器的老路")
	}
	if !errors.Is(err, ErrBrowserBusy) {
		t.Errorf("应套上 ErrBrowserBusy 以便调用方判定不可重试，实际：%v", err)
	}
	if !strings.Contains(err.Error(), port) {
		t.Errorf("错误文本里应出现被占用的端口号 %s 方便直接处置，实际：%v", port, err)
	}
}

func TestGuardBrowserStartupRejectsLiveProfileLock(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上进程存活探测不可用")
	}
	dir := t.TempDir()
	writeLock(t, dir, fmt.Sprintf("somehost-%d", os.Getpid()))

	// 端口是空闲的——这个用例专治"只看端口不够"：
	// 占位者可能用完全不同的调试端口，但 profile 锁骗不了人。
	err := guardBrowserStartup(freePort(t), dir)
	if err == nil {
		t.Fatal("profile 被活着的进程持有时没报错")
	}
	if !errors.Is(err, ErrBrowserBusy) {
		t.Errorf("应套上 ErrBrowserBusy，实际：%v", err)
	}
	if !strings.Contains(err.Error(), "profile") {
		t.Errorf("错误文本里应提到 profile，实际：%v", err)
	}
}

func TestBrowserBusyErrorMentionsDiagnosticTool(t *testing.T) {
	err := browserBusyError("9223", ".chrome-profile",
		portProbe{InUse: true, IsCDP: true}, "PID 12345（锁 h-12345）")
	msg := err.Error()
	for _, want := range []string{"9223", "CDP", ".chrome-profile", "PID 12345", "cdpdiag"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误文本应包含 %q，实际：%s", want, msg)
		}
	}
}
