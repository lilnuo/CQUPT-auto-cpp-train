package main

import (
	"context"
	"encoding/json"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"cqupt/train"
)

// ============================================================================
// api.go —— HTTP 层
//
// 职责边界：只做"协议翻译"——把 HTTP 请求翻译成对 store 的调用，
// 再把结果翻译成 JSON 或 HTML。它**不做业务判断**（那在 train 里），
// 也不直接写 SQL（那在 store 里）。
//
// 这一层最容易犯的错是"顺手多做一点"：在 handler 里拼一段 SQL、
// 或者写一点业务规则。一旦开了这个头，同样的规则很快会在第二个 handler
// 里被复制一遍，然后两处开始不一致。
// ============================================================================

// Server 持有依赖并提供路由。
type Server struct {
	store *Store
	cfg   Config
	log   *slog.Logger
}

// NewServer 构造 HTTP 服务。
func NewServer(store *Store, cfg Config, log *slog.Logger) *Server {
	return &Server{store: store, cfg: cfg, log: log}
}

// Handler 装配路由与中间件。
//
// 用标准库的 http.ServeMux 而不是引入 Gin/Echo：Go 1.22 起 ServeMux 已经支持
// 方法匹配和路径参数（"POST /api/tasks/{id}"），本项目需要的它全有。
// 少一个依赖就少一份版本升级与安全公告的负担。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("POST /api/tasks", s.handleCreateTask)
	mux.HandleFunc("GET /api/tasks", s.handleListTasks)
	mux.HandleFunc("GET /api/tasks/{id}", s.handleGetTask)
	mux.HandleFunc("GET /api/tasks/{id}/results", s.handleGetResults)
	mux.HandleFunc("DELETE /api/tasks/{id}", s.handleCancelTask)
	mux.HandleFunc("GET /", s.handleProgressPage)

	// 中间件的顺序是有讲究的，由外到内分别是：
	//   recover → 日志 → 超时 → 业务
	//
	// 1) recover 必须在最外层。它一旦落在日志里面，handler 里的 panic
	//    就会把日志中间件的收尾代码一起跳过，于是"请求日志"里永远看不到
	//    那条导致 500 的请求——恰恰是最需要看到的那条。
	// 2) 日志在超时外面，才能记录到"因为超时被中断"这个事实（状态码 504），
	//    以及真实的耗时。
	// 3) 超时在业务外面，保证任何 handler 都不会无限期占着连接。
	return s.recoverMW(s.logMW(s.timeoutMW(mux, 10*time.Second)))
}

// ---------------------------------------------------------------------------
// 中间件
// ---------------------------------------------------------------------------

// statusRecorder 包一层 ResponseWriter 以捕获状态码，供日志使用。
//
// http.ResponseWriter 本身不提供"我刚写了什么状态码"，而日志里没有状态码
// 基本等于没用。包装一层是标准做法。
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (r *statusRecorder) WriteHeader(code int) {
	if !r.wrote {
		r.status = code
		r.wrote = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if !r.wrote {
		// 没显式调用 WriteHeader 就直接 Write，等价于 200
		r.status = http.StatusOK
		r.wrote = true
	}
	return r.ResponseWriter.Write(b)
}

// logMW 记录每个请求的方法、路径、状态码与耗时。
//
// 刻意不记录请求体和查询串：本服务的请求体里有学生密码。
// 日志是"会被长期保存并广泛传播"的数据，往里写凭据等于把密码又泄一遍。
func (s *Server) logMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.log.Info("请求",
			"方法", r.Method,
			"路径", r.URL.Path,
			"状态", rec.status,
			"耗时", time.Since(start).Round(time.Millisecond),
		)
	})
}

// recoverMW 兜住 handler 里的 panic，避免一个请求把整个服务带崩。
//
// 没有它的话，任何一个 nil 解引用都会让 Go 的 http 服务器在那一瞬间
// 掐断连接，而且栈信息只进 stderr——进程还在，但客户端看到的是
// "connection reset"，非常难排查。
func (s *Server) recoverMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("处理请求时发生 panic",
					"方法", r.Method, "路径", r.URL.Path, "panic", rec)
				// 对外只给一句话，不回显 panic 内容与调用栈：
				// 那里面可能带路径、变量值等内部信息。
				writeJSON(w, http.StatusInternalServerError, map[string]string{
					"error": "服务器内部错误，请查看服务端日志",
				})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// timeoutMW 给每个请求套一个超时。
//
// 注意这个超时只保护"请求处理"这一段，和任务执行超时（TASK_MAX_RUNTIME）
// 是两回事：提交任务只写一行数据库就返回，本该毫秒级完成；
// 如果它跑了 10 秒还没返回，说明数据库出问题了，这时候快速失败比一直挂着好。
func (s *Server) timeoutMW(next http.Handler, d time.Duration) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), d)
		defer cancel()

		// 只挂 ctx 是不够的：handler 必须自己检查 ctx 才会被打断，
		// 而 database/sql 会检查、纯计算不会。所以超时后要由中间件兜底。
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r.WithContext(ctx))

		// handler 一个字都没写就撞上超时，说明它卡在了会理会 ctx 的地方
		// （多半是数据库），此时补一个 504 让客户端知道该重试。
		// 已经写过响应就绝不能再写——那会往响应体后面追加一段 JSON，
		// 把原本的响应彻底搞坏。
		if errors.Is(ctx.Err(), context.DeadlineExceeded) && !rec.wrote {
			writeJSON(w, http.StatusGatewayTimeout, map[string]string{
				"error": "请求处理超时，请稍后重试",
			})
		}
	})
}

// ---------------------------------------------------------------------------
// 请求 / 响应结构
// ---------------------------------------------------------------------------

// createTaskRequest 是提交任务的请求体。
type createTaskRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Mode     string `json:"mode"`
	Num      int    `json:"num"`
}

// taskView 是对外暴露的任务视图。
//
// 专门定义它、而不是直接把 store.Task 序列化出去，有两个原因：
//   - store.Task 里有 PasswordEnc，一旦忘了加 json:"-" 就是灾难，
//     用独立视图结构则天然不会泄漏；
//   - 视图字段可以随接口版本演进，不必牵动内部结构。
//
// 这种"内外结构分离"看起来啰嗦，但它把"泄漏一个字段"从
// "靠记性"变成了"结构上不可能"。
type taskView struct {
	ID           int64       `json:"id"`
	Username     string      `json:"username"`
	Mode         string      `json:"mode"`
	Num          int         `json:"num"`
	Status       string      `json:"status"`
	Score        *int        `json:"score"`
	QuestionDone int         `json:"question_done"`
	Error        string      `json:"error,omitempty"`
	TryCount     int         `json:"try_count"`
	MaxTry       int         `json:"max_try"`
	CreatedAt    string      `json:"created_at"`
	StartedAt    *string     `json:"started_at,omitempty"`
	FinishedAt   *string     `json:"finished_at,omitempty"`
	Events       []eventView `json:"events,omitempty"`
}

type eventView struct {
	Seq    int     `json:"seq"`
	Kind   string  `json:"kind"`
	Detail string  `json:"detail"`
	Pid    string  `json:"pid,omitempty"`
	Score  float64 `json:"score,omitempty"`
	At     string  `json:"at"`
}

type resultView struct {
	Pid      string  `json:"pid"`
	Question string  `json:"question,omitempty"`
	Answer   string  `json:"answer,omitempty"`
	Score    float64 `json:"score"`
	Tries    int     `json:"tries"`
	Passed   bool    `json:"passed"`
}

var timeLayout = "2006-01-02 15:04:05"

func fmtTime(t time.Time) string { return t.Format(timeLayout) }

func fmtTimePtr(t *time.Time) *string {
	if t == nil {
		return nil
	}
	s := fmtTime(*t)
	return &s
}

func viewTask(t *Task, events []EventRow) taskView {
	v := taskView{
		ID:           t.ID,
		Username:     t.Username,
		Mode:         t.Mode,
		Num:          t.Num,
		Status:       string(t.Status),
		Score:        t.Score,
		QuestionDone: t.QuestionDone,
		Error:        t.Error,
		TryCount:     t.TryCount,
		MaxTry:       t.MaxTry,
		CreatedAt:    fmtTime(t.CreatedAt),
		StartedAt:    fmtTimePtr(t.StartedAt),
		FinishedAt:   fmtTimePtr(t.FinishedAt),
	}
	for _, e := range events {
		v.Events = append(v.Events, eventView{
			Seq: e.Seq, Kind: e.Kind, Detail: e.Detail,
			Pid: e.Pid, Score: e.Score, At: fmtTime(e.At),
		})
	}
	return v
}

// ---------------------------------------------------------------------------
// handlers
// ---------------------------------------------------------------------------

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	// 探活不能只看"进程还在"，要真的碰一下数据库——
	// 否则数据库挂了而 HTTP 还在，负载均衡器会一直把流量送过来。
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if _, err := s.store.StatusCount(ctx); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status": "degraded", "error": "数据库不可用",
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	var req createTaskRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体不是合法 JSON: " + err.Error()})
		return
	}

	// 先构造 train.Request 复用它的校验规则——这批规则属于业务，
	// 不该在 HTTP 层再实现一遍，否则两边迟早不一致。
	tr := train.Request{
		Username: strings.TrimSpace(req.Username),
		Password: req.Password,
		Num:      req.Num,
		Mode:     strings.TrimSpace(req.Mode),
	}.Normalize()
	if err := tr.Validate(); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	// 密码只在这里以明文存在一瞬间：立刻加密，明文不落库、不进日志。
	// 加密动作放在 api 层而不是 store 层，是为了让 store 的接口
	// 签名上就只有"密文"这一种可能——它想存明文都存不了。
	enc, err := EncryptPassword(s.cfg.TaskSecret, tr.Password)
	if err != nil {
		s.log.Error("加密任务密码失败", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "加密失败"})
		return
	}

	id, err := s.store.CreateTask(r.Context(), tr.Username, tr.Mode, tr.Num, enc, defaultMaxTry)
	if err != nil {
		s.log.Error("创建任务失败", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "创建任务失败"})
		return
	}

	s.log.Info("已接受任务", "task_id", id, "学号", maskUsername(tr.Username), "模式", tr.Mode, "题量", tr.Num)
	// 202 Accepted：请求已被接受，但还没处理完——这正是队列的语义。
	// 返回 200 会让人以为任务已经跑完了。
	writeJSON(w, http.StatusAccepted, map[string]any{
		"id":     id,
		"status": string(StatusPending),
		"提示":     "任务已入队，可用 GET /api/tasks/" + strconv.FormatInt(id, 10) + " 查询进度",
	})
}

func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	tasks, err := s.store.ListTasks(r.Context(), limit)
	if err != nil {
		s.log.Error("查询任务列表失败", "err", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "查询失败"})
		return
	}
	counts, err := s.store.StatusCount(r.Context())
	if err != nil {
		s.log.Warn("统计任务状态失败", "err", err)
	}

	views := make([]taskView, 0, len(tasks))
	for _, t := range tasks {
		views = append(views, viewTask(t, nil))
	}
	byStatus := map[string]int{}
	for k, v := range counts {
		byStatus[string(k)] = v
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tasks": views,
		"按状态统计": byStatus,
	})
}

func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	id, ok := s.parseID(w, r)
	if !ok {
		return
	}
	t, err := s.store.GetTask(r.Context(), id)
	if errors.Is(err, ErrTaskNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "任务不存在"})
		return
	}
	if err != nil {
		s.log.Error("查询任务失败", "err", err, "task_id", id)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "查询失败"})
		return
	}
	events, err := s.store.ListEvents(r.Context(), id)
	if err != nil {
		s.log.Warn("查询任务事件失败", "err", err, "task_id", id)
	}
	writeJSON(w, http.StatusOK, viewTask(t, events))
}

func (s *Server) handleGetResults(w http.ResponseWriter, r *http.Request) {
	id, ok := s.parseID(w, r)
	if !ok {
		return
	}
	rows, err := s.store.ListResults(r.Context(), id)
	if err != nil {
		s.log.Error("查询题目结果失败", "err", err, "task_id", id)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "查询失败"})
		return
	}
	views := make([]resultView, 0, len(rows))
	for _, x := range rows {
		views = append(views, resultView{
			Pid: x.Pid, Question: x.Question, Answer: x.Answer,
			Score: x.Score, Tries: x.Tries, Passed: x.Passed,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"task_id": id, "results": views})
}

func (s *Server) handleCancelTask(w http.ResponseWriter, r *http.Request) {
	id, ok := s.parseID(w, r)
	if !ok {
		return
	}
	if err := s.store.CancelTask(r.Context(), id); err != nil {
		// 这里不区分"不存在"和"状态不允许"，都是 409：调用方拿到的信息
		// 足够采取下一步动作（去查一下状态），多说反而暴露内部状态。
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
		return
	}
	s.log.Info("任务已取消", "task_id", id)
	writeJSON(w, http.StatusOK, map[string]string{"status": string(StatusCanceled)})
}

// ---------------------------------------------------------------------------
// 进度页
// ---------------------------------------------------------------------------

var progressTmpl = template.Must(template.New("progress").Parse(`<!DOCTYPE html>
<html lang="zh-CN"><head><meta charset="utf-8">
<title>CQUPT 刷题服务</title>
<meta http-equiv="refresh" content="5">
<style>
 body{font-family:-apple-system,"PingFang SC",sans-serif;margin:2rem;color:#222;background:#fafafa}
 h1{font-size:1.2rem;font-weight:500}
 table{border-collapse:collapse;width:100%;background:#fff;font-size:.86rem}
 th,td{border:1px solid #e3e3e3;padding:.45rem .6rem;text-align:left;vertical-align:top}
 th{background:#f2f2f2;font-weight:500}
 .s-pending{color:#8a6d00}.s-running{color:#0b5}.s-succeeded{color:#0a0}
 .s-failed{color:#c00}.s-canceled{color:#888}
 code{font-size:.8rem}
 .sum{display:flex;gap:1.2rem;margin:.8rem 0 1.2rem;font-size:.85rem;color:#555}
</style></head><body>
<h1>CQUPT 刷题服务 · 任务进度</h1>
<div class="sum">
 <span>排队 {{index .Counts "pending"}}</span>
 <span>执行中 {{index .Counts "running"}}</span>
 <span>成功 {{index .Counts "succeeded"}}</span>
 <span>失败 {{index .Counts "failed"}}</span>
</div>
<table>
<tr><th>ID</th><th>学号</th><th>模式</th><th>题量</th><th>状态</th><th>进度</th>
    <th>总分</th><th>领取次数</th><th>创建时间</th><th>错误</th></tr>
{{range .Tasks}}
<tr>
 <td>{{.ID}}</td><td>{{.Username}}</td><td>{{.Mode}}</td><td>{{.Num}}</td>
 <td class="s-{{.Status}}">{{.Status}}</td>
 <td>{{.QuestionDone}}/{{.Num}}</td>
 <td>{{if .Score}}{{.Score}}{{else}}-{{end}}</td>
 <td>{{.TryCount}}/{{.MaxTry}}</td>
 <td>{{.CreatedAt}}</td>
 <td><code>{{.Error}}</code></td>
</tr>
{{else}}
<tr><td colspan="10">还没有任务。用 POST /api/tasks 提交一个。</td></tr>
{{end}}
</table>
<p style="font-size:.78rem;color:#888">页面每 5 秒自动刷新 · 仅监听本机，未做鉴权</p>
</body></html>`))

// handleProgressPage 渲染一个只读的任务总览页。
//
// 这里必须用 html/template，不能用字符串拼接。页面上有学号、错误信息、
// 题干这些来自请求的数据；用 text/template 或手工拼串，一个学号里塞进
// <script> 就是一次存储型 XSS。html/template 会对 HTML 上下文自动转义，
// 把"记得转义"从人的责任变成框架的责任。
func (s *Server) handleProgressPage(w http.ResponseWriter, r *http.Request) {
	// 只处理根路径，其余交给 404，避免这个兜底路由把 API 的 404 也吞掉
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	tasks, err := s.store.ListTasks(r.Context(), 50)
	if err != nil {
		http.Error(w, "查询任务失败", http.StatusInternalServerError)
		return
	}
	// 页面和 JSON 接口共用同一套视图转换。
	//
	// 不共用的话两边会慢慢长歪：最初的版本直接把 []*store.Task 丢给模板，
	// 于是同一个 created_at 在 JSON 里是 "2026-09-25 11:27:37"、
	// 在页面上却是 "2026-09-25 11:27:37.383 +0800 CST"——
	// 因为模板拿到的是裸 time.Time。同一份数据两种长相，看的人会先怀疑数据。
	views := make([]taskView, 0, len(tasks))
	for _, t := range tasks {
		views = append(views, viewTask(t, nil))
	}

	counts, err := s.store.StatusCount(r.Context())
	if err != nil {
		s.log.Warn("统计任务状态失败", "err", err)
	}
	countMap := map[string]int{}
	for k, v := range counts {
		countMap[string(k)] = v
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := progressTmpl.Execute(w, map[string]any{
		"Tasks":  views,
		"Counts": countMap,
	}); err != nil {
		s.log.Error("渲染进度页失败", "err", err)
	}
}

// ---------------------------------------------------------------------------
// 小工具
// ---------------------------------------------------------------------------

// defaultMaxTry 是新任务的默认最大领取次数。
const defaultMaxTry = 2

// parseID 从路径里取出任务 id，失败时已经写过响应，返回 ok=false。
func (s *Server) parseID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	raw := r.PathValue("id")
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "任务 id 必须是正整数"})
		return 0, false
	}
	return id, true
}

// writeJSON 统一 JSON 响应出口。
//
// 先设置 Content-Type 再 WriteHeader，顺序反了就改不了了；
// 所有响应都从这里出，就不必在每个 handler 里记这条规则。
func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		// 走到这里说明响应头已经发出去了，状态码改不了、也没法重写响应，
		// 唯一能做的就是留下痕迹：客户端多半会看到一个被截断的 JSON，
		// 排查时得知道这是"写失败"而不是"服务端本来就这么返回"。
		slog.Warn("写响应体失败", "err", err, "状态码", code)
	}
}
