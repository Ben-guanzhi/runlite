// Package api 是 daemon 的本地 HTTP 面：GUI、CLI、MCP 都走这里，保证状态唯一。
package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"runlite/internal/config"
	"runlite/internal/env"
	"runlite/internal/logs"
	"runlite/internal/proc"
	"runlite/internal/project"
	"runlite/internal/service"
)

type Server struct {
	svc        *service.Service
	token      string
	ln         net.Listener
	http       *http.Server
	onShutdown func()
}

func NewToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "runlite-insecure-token"
	}
	return hex.EncodeToString(b[:])
}

func New(svc *service.Service, token string) *Server {
	s := &Server{svc: svc, token: token}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", s.health)
	mux.HandleFunc("GET /api/doctor", s.doctor)
	mux.HandleFunc("GET /api/projects", s.listProjects)
	mux.HandleFunc("POST /api/projects", s.addProject)
	mux.HandleFunc("DELETE /api/projects/{key}", s.removeProject)
	mux.HandleFunc("GET /api/projects/{key}", s.getProject)
	mux.HandleFunc("POST /api/projects/{key}/start", s.startProject)
	mux.HandleFunc("POST /api/projects/{key}/stop", s.stopProject)
	mux.HandleFunc("POST /api/projects/{key}/restart", s.restartProject)
	mux.HandleFunc("GET /api/projects/{key}/logs", s.projectLogs)
	mux.HandleFunc("GET /api/runs", s.listRuns)
	mux.HandleFunc("POST /api/prune", s.prune)
	mux.HandleFunc("POST /api/shutdown", s.shutdown)
	s.http = &http.Server{Handler: s.auth(mux)}
	return s
}

// auth 只允许带 token 的本机调用；拒绝带浏览器 Origin 的请求，避免网页里一段 JS
// 就能操作本机进程（端口虽然只绑 127.0.0.1，但浏览器可以访问本机回环）。
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" && !strings.HasPrefix(origin, "runlite://") {
			writeErr(w, http.StatusForbidden, errors.New("拒绝来自浏览器来源的调用"))
			return
		}
		if r.URL.Path != "/api/health" && r.Header.Get("X-Runlite-Token") != s.token {
			writeErr(w, http.StatusUnauthorized, errors.New("token 缺失或不正确"))
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) Serve() error { return s.http.Serve(s.ln) }

func (s *Server) OnShutdown(f func()) { s.onShutdown = f }

func (s *Server) Shutdown(ctx context.Context) error { return s.http.Shutdown(ctx) }

// Listen 优先固定端口失败则让系统分配，返回实际端口。
// ErrDaemonExists 表示首选端口上已经有一个 runlite daemon 在服务。
// 第二个 daemon 必须拒绝启动：两个进程各有一份内存状态，GUI/CLI/MCP 就会看到互相
// 矛盾的项目状态，那正是"daemon 作为唯一事实源"要消除的东西。
var ErrDaemonExists = errors.New("该端口上已有 runlite daemon 在运行")

// Probe 不带 token 探一下某端口是不是 runlite daemon（/api/health 本来就免鉴权），
// 顺带取回它的主目录：两个 daemon 抢同一端口时，只有 home 能说明"那是谁的 daemon"。
// 探不到就返回 pid 0。
func Probe(port int, timeout time.Duration) (int, string) {
	c := http.Client{Timeout: timeout}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/api/health", port))
	if err != nil {
		return 0, ""
	}
	defer resp.Body.Close()
	var out struct {
		OK   bool   `json:"ok"`
		PID  int    `json:"pid"`
		Home string `json:"home"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || !out.OK {
		return 0, ""
	}
	return out.PID, out.Home
}

func (s *Server) Listen(preferred int) (int, error) {
	addr := fmt.Sprintf("127.0.0.1:%d", preferred)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		// 绑不上先看是不是同行：是的话直接拒绝，不写 daemon.json，不去抢事实源。
		if pid, _ := Probe(preferred, 2*time.Second); pid > 0 {
			return 0, fmt.Errorf("%w（pid %d，端口 %d）", ErrDaemonExists, pid, preferred)
		}
		// 否则是被别的程序占了，退回临时端口，保证本机还能用一个 daemon。
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			return 0, err
		}
	}
	s.ln = ln
	return ln.Addr().(*net.TCPAddr).Port, nil
}

type ProjectView struct {
	ID      string                 `json:"id"`
	Name    string                 `json:"name"`
	Dir     string                 `json:"dir"`
	Kind    string                 `json:"kind"`
	Active  string                 `json:"active"`
	Configs []project.LaunchConfig `json:"configs"`
	Notes   []string               `json:"notes,omitempty"`
	Run     *proc.Run              `json:"run,omitempty"`
}

func (s *Server) view(p *project.Project) ProjectView {
	v := ProjectView{ID: p.ID, Name: p.Name, Dir: p.Dir, Kind: string(p.Kind), Active: p.Active, Configs: p.Configs, Notes: p.Notes}
	// 正在跑的那次优先；没有就跑完最近一次，让 failed/zombie 也能被看见，
	// 而不是统一含糊成"停止"。
	if r, ok := s.svc.RunByID(p.ID); ok {
		v.Run = r
	} else if r, ok := s.svc.LastRun(p.ID); ok {
		v.Run = r
	}
	return v
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": Version, "pid": os.Getpid(), "home": s.svc.Home()})
}

func (s *Server) doctor(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		s.svc.InvalidateReport()
	}
	writeJSON(w, http.StatusOK, s.svc.Report())
}

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) {
	ps := s.svc.Projects()
	out := make([]ProjectView, 0, len(ps))
	for _, p := range ps {
		out = append(out, s.view(p))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) addProject(w http.ResponseWriter, r *http.Request) {
	var req service.AddRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	p, err := s.svc.Add(req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, s.view(p))
}

func (s *Server) removeProject(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.Remove(r.PathValue("key")); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"removed": true})
}

func (s *Server) getProject(w http.ResponseWriter, r *http.Request) {
	p, err := s.svc.Resolve(r.PathValue("key"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, s.view(p))
}

func (s *Server) startProject(w http.ResponseWriter, r *http.Request) {
	var req service.StartRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	req.Key = r.PathValue("key")
	run, err := s.svc.Start(req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if wait := r.URL.Query().Get("wait"); wait == "1" {
		run = s.waitRun(run.ProjectID, run.ID)
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) waitRun(projectID, runID string) *proc.Run {
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		r, ok := s.svc.RunByID(projectID)
		if !ok {
			if rs := s.svc.Runs(); len(rs) > 0 && rs[0].ID == runID {
				return rs[0]
			}
			return nil
		}
		if r.ID != runID {
			return r
		}
		switch r.State {
		case proc.StateRunning, proc.StateFailed, proc.StateStopped, proc.StateZombie:
			return r
		case proc.StateStarting:
			// daemon 已经给出"启动比预期慢"的结论，再干等没有意义：
			// 把当前事实交回客户端，由它决定继续查还是先去做别的。
			if r.Reason != "" {
				return r
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	r, _ := s.svc.RunByID(projectID)
	return r
}

func (s *Server) stopProject(w http.ResponseWriter, r *http.Request) {
	force := r.URL.Query().Get("force") == "1"
	run, err := s.svc.Stop(r.PathValue("key"), force)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) restartProject(w http.ResponseWriter, r *http.Request) {
	var req service.StartRequest
	_ = json.NewDecoder(r.Body).Decode(&req)
	req.Key = r.PathValue("key")
	run, err := s.svc.Restart(req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) projectLogs(w http.ResponseWriter, r *http.Request) {
	n := intQuery(r, "n", 200)
	since := intQuery(r, "since", 0)
	run, lines, err := s.svc.Logs(r.PathValue("key"), n, since, r.URL.Query().Get("grep"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"run": run, "lines": lines})
}

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.svc.Runs())
}

func (s *Server) prune(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"pruned": s.svc.Prune()})
}

// shutdown 只结束 daemon 自身。被管理的项目进程是独立进程，不随之退出——
// 这正是 daemon 存在的理由，所以这里刻意不做任何 KillTree。
func (s *Server) shutdown(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"bye": true})
	if s.onShutdown != nil {
		go func() {
			time.Sleep(120 * time.Millisecond)
			s.onShutdown()
		}()
	}
}

func intQuery(r *http.Request, key string, def int) int {
	if v := r.URL.Query().Get(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]any{"error": err.Error()})
}

// Version 是唯一版本号来源：/api/health 报它、`runlite version` 打印它，
// build.ps1 也从这里读它来给产物命名，避免"zip 叫 0.1.0、里面的 exe 说自己是 0.0.9"。
const Version = "0.1.0"

// Client 供 CLI/GUI/MCP 复用。base+path 只来自本机 daemon.json，不接受外部 URL。
type Client struct {
	base  string
	token string
	http  *http.Client
}

func NewClient(info *config.DaemonInfo) *Client {
	return &Client{
		base:  fmt.Sprintf("http://127.0.0.1:%d", info.Port),
		token: info.Token,
		http:  &http.Client{Timeout: 3 * time.Minute},
	}
}

func (c *Client) Do(method, path string, body any, out any) error {
	var bodyReader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		bodyReader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, c.base+path, bodyReader)
	if err != nil {
		return err
	}
	req.Header.Set("X-Runlite-Token", c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// 流式解析：daemon 端本来就 json.NewEncoder(w).Encode 一次写出去，客户端
	// 不需要 io.ReadAll 再 json.Unmarshal 两次全量拷贝；大日志响应时那层多余的
	// []byte 是最显眼的一次性峰值。同时把错误分支留在 ReadAll 上（错误体通常很小，
	// 而且错误消息本身需要完整读出来当字符串给用户）。
	if resp.StatusCode >= 400 {
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
		if err != nil {
			return err
		}
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		if e.Error != "" {
			return errors.New(e.Error)
		}
		return fmt.Errorf("daemon 返回 %s: %s", resp.Status, strings.TrimSpace(string(raw)))
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("解析 daemon 响应失败: %w", err)
	}
	return nil
}

func (c *Client) Health() error { return c.Do(http.MethodGet, "/api/health", nil, nil) }

func (c *Client) Shutdown() error {
	return c.Do(http.MethodPost, "/api/shutdown", nil, nil)
}

func (c *Client) Projects() ([]ProjectView, error) {
	var out []ProjectView
	return out, c.Do(http.MethodGet, "/api/projects", nil, &out)
}

func (c *Client) Add(req service.AddRequest) (ProjectView, error) {
	var out ProjectView
	return out, c.Do(http.MethodPost, "/api/projects", req, &out)
}

func (c *Client) Remove(key string) error {
	return c.Do(http.MethodDelete, "/api/projects/"+url.PathEscape(key), nil, nil)
}

func (c *Client) Start(key string, req service.StartRequest, wait bool) (*proc.Run, error) {
	var out proc.Run
	path := "/api/projects/" + url.PathEscape(key) + "/start"
	if wait {
		path += "?wait=1"
	}
	return &out, c.Do(http.MethodPost, path, req, &out)
}

func (c *Client) Stop(key string, force bool) (*proc.Run, error) {
	path := "/api/projects/" + url.PathEscape(key) + "/stop"
	if force {
		path += "?force=1"
	}
	var out proc.Run
	return &out, c.Do(http.MethodPost, path, nil, &out)
}

func (c *Client) Project(key string) (ProjectView, error) {
	var out ProjectView
	return out, c.Do(http.MethodGet, "/api/projects/"+url.PathEscape(key), nil, &out)
}

type LogsResponse struct {
	Run   *proc.Run   `json:"run"`
	Lines []logs.Line `json:"lines"`
}

func (c *Client) Logs(key string, n, since int, grep string) (*LogsResponse, error) {
	q := url.Values{}
	q.Set("n", strconv.Itoa(n))
	q.Set("since", strconv.Itoa(since))
	if grep != "" {
		q.Set("grep", grep)
	}
	var out LogsResponse
	err := c.Do(http.MethodGet, "/api/projects/"+url.PathEscape(key)+"/logs?"+q.Encode(), nil, &out)
	return &out, err
}

func (c *Client) Doctor() (*env.Report, error) {
	var out env.Report
	return &out, c.Do(http.MethodGet, "/api/doctor", nil, &out)
}
