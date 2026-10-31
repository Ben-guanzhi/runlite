// Package mcp 把 runlite 以 MCP（JSON-RPC 2.0 over stdio）面暴露给 Agent。
// 它自己不持有状态：所有动作都转给 daemon 的 HTTP 面，事实源仍然只有一个。
package mcp

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"

	"runlite/internal/api"
	"runlite/internal/env"
	"runlite/internal/proc"
	"runlite/internal/project"
	"runlite/internal/service"
)

// ProtocolVersion 是与主流 Agent 协商时回落的版本；客户端要别的版本时原样回它要的，
// 只在它没给版本号时落到这个值。
const ProtocolVersion = "2024-11-05"

const serverName = "runlite"

// serverVersion mirrors the API's Version constant — one source of truth, so
// bumping the version does not leave the MCP serverInfo behind.
const serverVersion = api.Version

// Backend 是 *api.Client 提供的那组动作；单测用它做替身。
type Backend interface {
	Projects() ([]api.ProjectView, error)
	Project(key string) (api.ProjectView, error)
	Start(key string, req service.StartRequest, wait bool) (*proc.Run, error)
	Stop(key string, force bool) (*proc.Run, error)
	Logs(key string, n, since int, grep string) (*api.LogsResponse, error)
	Add(req service.AddRequest) (api.ProjectView, error)
	Remove(key string) error
	Doctor() (*env.Report, error)
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

const (
	codeParseError  = -32700
	codeInvalidReq  = -32600
	codeNoMethod    = -32601
	codeBadParams   = -32602
	codeInternalErr = -32603
)

// Serve 逐行读 stdin、逐行写 stdout，直到输入结束（Agent 关掉管道就是退出信号）。
// stderr 留给人/日志，不参与协议。
func Serve(in io.Reader, out io.Writer, b Backend) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	enc := json.NewEncoder(out)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		resp, reply := handle(line, b)
		if !reply {
			continue
		}
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
	return sc.Err()
}

func handle(raw []byte, b Backend) (rpcResponse, bool) {
	var req rpcRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return rpcResponse{JSONRPC: "2.0", Error: &rpcError{Code: codeParseError, Message: "不是合法的 JSON-RPC 消息"}}, true
	}
	hasID := len(req.ID) > 0 && string(req.ID) != "null"
	if req.JSONRPC != "2.0" {
		if !hasID {
			return rpcResponse{}, false
		}
		return rpcResponse{JSONRPC: "2.0", ID: req.ID,
			Error: &rpcError{Code: codeInvalidReq, Message: `缺 "jsonrpc":"2.0"`}}, true
	}
	// 通知（没有 id）不允许回包，否则客户端会把响应错认成上一条请求的结果。
	if !hasID {
		return rpcResponse{}, false
	}
	switch req.Method {
	case "initialize":
		return rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: initialize(req.Params)}, true
	case "ping":
		return rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{}}, true
	case "tools/list":
		return rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"tools": toolList()}}, true
	case "tools/call":
		res, err := callTool(b, req.Params)
		if err != nil {
			var pe *paramError
			if isParam(err, &pe) {
				return rpcResponse{JSONRPC: "2.0", ID: req.ID,
					Error: &rpcError{Code: codeBadParams, Message: err.Error()}}, true
			}
			return rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: errorContent(err)}, true
		}
		return rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: res}, true
	default:
		return rpcResponse{JSONRPC: "2.0", ID: req.ID,
			Error: &rpcError{Code: codeNoMethod, Message: "未知方法: " + req.Method}}, true
	}
}

func initialize(params json.RawMessage) map[string]any {
	version := ProtocolVersion
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if json.Unmarshal(params, &p) == nil && p.ProtocolVersion != "" {
		version = p.ProtocolVersion
	}
	return map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": serverName, "version": serverVersion},
	}
}

// paramError 区分"参数不对"（协议层错误）与"项目起不来"（工具结果里的 isError）。
// Agent 靠这两者的差别决定是改参数还是去查日志。
type paramError struct{ msg string }

func (e *paramError) Error() string { return e.msg }

func isParam(err error, target **paramError) bool {
	if pe, ok := err.(*paramError); ok {
		*target = pe
		return true
	}
	return false
}

type tool struct {
	name        string
	description string
	required    []string
	props       map[string][]string
	call        func(b Backend, args map[string]any) (string, error)
}

func obj(v any) string {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(raw)
}

func text(content string, isError bool) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": content}}, "isError": isError}
}

func errorContent(err error) map[string]any {
	return text("失败: "+err.Error(), true)
}

var tools = []tool{
	{
		name:        "list_projects",
		description: "列出本机已注册的项目及其当前状态（正在跑/上次结果）。",
		props:       map[string][]string{},
		call: func(b Backend, _ map[string]any) (string, error) {
			ps, err := b.Projects()
			if err != nil {
				return "", err
			}
			rows := make([]map[string]any, 0, len(ps))
			for _, p := range ps {
				row := map[string]any{"id": p.ID, "name": p.Name, "dir": p.Dir, "kind": p.Kind,
					"active": p.Active, "configs": configIDs(p.Configs)}
				if p.Run != nil {
					row["state"] = string(p.Run.State)
					row["pid"] = p.Run.PID
					row["port"] = p.Run.Port
					row["reason"] = p.Run.Reason
				}
				rows = append(rows, row)
			}
			return obj(rows), nil
		},
	},
	{
		name:        "add_project",
		description: "注册一个目录为可启动项目。给了 cmd 就按 exec 注册，否则自动检测。",
		required:    []string{"dir"},
		props: map[string][]string{
			"dir":     {"string", "项目绝对路径"},
			"name":    {"string", "显示名，默认取构建文件里的名字"},
			"cmd":     {"string", "跳过检测，直接指定启动命令行"},
			"profile": {"string", "spring.profiles.active / NODE_ENV"},
			"port":    {"integer", "覆盖自动探测的端口"},
		},
		call: func(b Backend, args map[string]any) (string, error) {
			port, err := intArg(args, "port")
			if err != nil {
				return "", err
			}
			v, err := b.Add(service.AddRequest{
				Dir:     strArg(args, "dir"),
				Name:    strArg(args, "name"),
				Cmd:     strArg(args, "cmd"),
				Profile: strArg(args, "profile"),
				Port:    port,
			})
			if err != nil {
				return "", err
			}
			return obj(v), nil
		},
	},
	{
		name:        "start_project",
		description: "启动项目。默认等到就绪（就绪靠启动日志或端口监听判定），大项目首次启动可能要两三分钟。",
		required:    []string{"project"},
		props: map[string][]string{
			"project": {"string", "项目 id、id 前缀、名字或所在目录"},
			"config":  {"string", "启动配置 id，默认用项目的当前配置"},
			"wait":    {"boolean", "false 时立即返回 starting"},
		},
		call: func(b Backend, args map[string]any) (string, error) {
			key := strArg(args, "project")
			if key == "" {
				return "", &paramError{"缺少 project"}
			}
			r, err := b.Start(key, service.StartRequest{Key: key, Config: strArg(args, "config")}, boolArg(args, "wait", true))
			if err != nil {
				return "", err
			}
			return obj(runView(r)), nil
		},
	},
	{
		name:        "stop_project",
		description: "确保项目停下来。项目本来没在跑也算成功，结果里说明是重复操作。",
		required:    []string{"project"},
		props: map[string][]string{
			"project": {"string", "项目 id、id 前缀、名字或所在目录"},
			"force":   {"boolean", "默认 true：直接结束进程树（本机没有优雅停下的通道）"},
		},
		call: func(b Backend, args map[string]any) (string, error) {
			key := strArg(args, "project")
			if key == "" {
				return "", &paramError{"缺少 project"}
			}
			r, err := b.Stop(key, boolArg(args, "force", true))
			if err != nil {
				return "", err
			}
			return obj(runView(r)), nil
		},
	},
	{
		name:        "project_status",
		description: "查一个项目的详情：配置清单、当前或上次运行的状态、pid、端口、日志路径。",
		required:    []string{"project"},
		props: map[string][]string{
			"project": {"string", "项目 id、id 前缀、名字或所在目录"},
		},
		call: func(b Backend, args map[string]any) (string, error) {
			key := strArg(args, "project")
			if key == "" {
				return "", &paramError{"缺少 project"}
			}
			v, err := b.Project(key)
			if err != nil {
				return "", err
			}
			return obj(v), nil
		},
	},
	{
		name:        "read_logs",
		description: "取项目最近日志的尾部，可用正则过滤。排查启动失败先看这个。",
		required:    []string{"project"},
		props: map[string][]string{
			"project": {"string", "项目 id、id 前缀、名字或所在目录"},
			"lines":   {"integer", "取最后多少行，默认 200"},
			"grep":    {"string", "按正则过滤"},
		},
		call: func(b Backend, args map[string]any) (string, error) {
			key := strArg(args, "project")
			if key == "" {
				return "", &paramError{"缺少 project"}
			}
			n, err := intArg(args, "lines")
			if err != nil {
				return "", err
			}
			if n == 0 {
				n = 200
			}
			res, err := b.Logs(key, n, 0, strArg(args, "grep"))
			if err != nil {
				return "", err
			}
			lines := make([]string, 0, len(res.Lines))
			for _, l := range res.Lines {
				lines = append(lines, l.Text)
			}
			return joinHeader(res.Run, lines), nil
		},
	},
	{
		name:        "remove_project",
		description: "从注册表里移除项目（不会结束正在跑的进程，要先 stop）。",
		required:    []string{"project"},
		props: map[string][]string{
			"project": {"string", "项目 id、id 前缀、名字"},
		},
		call: func(b Backend, args map[string]any) (string, error) {
			key := strArg(args, "project")
			if key == "" {
				return "", &paramError{"缺少 project"}
			}
			if err := b.Remove(key); err != nil {
				return "", err
			}
			return "已移除 " + key, nil
		},
	},
	{
		name:        "doctor",
		description: "报告本机工具链（java/mvn/gradle/node 等）是否可用及版本。",
		props:       map[string][]string{},
		call: func(b Backend, _ map[string]any) (string, error) {
			r, err := b.Doctor()
			if err != nil {
				return "", err
			}
			return obj(r), nil
		},
	},
}

func joinHeader(r *proc.Run, lines []string) string {
	head := "（没有运行记录）"
	if r != nil {
		head = fmt.Sprintf("%s  state=%s pid=%d port=%d  日志: %s", r.Name, r.State, r.PID, r.Port, r.LogPath)
	}
	if len(lines) == 0 {
		return head + "\n（日志为空）"
	}
	// strings.Builder 一遍过：老写法 out += l+"\n" 每次迭代都全量重拼，
	// read_logs 一次可能拼几十行日志，那个 O(n²) 的分配是白交的。
	var b strings.Builder
	b.Grow(len(head) + len(lines)*32 + len(lines)) // 平均行长 + 换行
	b.WriteString(head)
	b.WriteByte('\n')
	for _, l := range lines {
		b.WriteString(l)
		b.WriteByte('\n')
	}
	return b.String()
}

func configIDs(cfgs []project.LaunchConfig) []string {
	out := make([]string, 0, len(cfgs))
	for _, c := range cfgs {
		out = append(out, c.ID)
	}
	return out
}

func runView(r *proc.Run) map[string]any {
	if r == nil {
		return map[string]any{}
	}
	return map[string]any{"project": r.ProjectID, "name": r.Name, "state": string(r.State),
		"pid": r.PID, "port": r.Port, "exitCode": r.ExitCode, "reason": r.Reason,
		"logPath": r.LogPath, "noOp": r.NoOp}
}

// toolList 是静态的（tools 在包级 var 里定义，不会变），预先算好一次：
// 老写法每次 tools/list 都重拼 map 切片，Agent 启动时调一次不算事，但白分配的
// 内存本来可以省。sync.Once 保证只算一次，后续调用直接返回缓存。
var (
	toolListOnce  sync.Once
	toolListCache []map[string]any
)

func toolList() []map[string]any {
	toolListOnce.Do(func() {
		toolListCache = make([]map[string]any, 0, len(tools))
		for _, t := range tools {
			props := map[string]any{}
			for name, d := range t.props {
				props[name] = map[string]any{"type": d[0], "description": d[1]}
			}
			required := t.required
			if required == nil {
				required = []string{}
			}
			toolListCache = append(toolListCache, map[string]any{
				"name":        t.name,
				"description": t.description,
				"inputSchema": map[string]any{"type": "object", "properties": props, "required": required,
					"additionalProperties": false},
			})
		}
		sort.Slice(toolListCache, func(i, j int) bool { return toolListCache[i]["name"].(string) < toolListCache[j]["name"].(string) })
	})
	return toolListCache
}

func callTool(b Backend, params json.RawMessage) (map[string]any, error) {
	var p struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, &paramError{"tools/call 参数解析失败: " + err.Error()}
	}
	for _, t := range tools {
		if t.name != p.Name {
			continue
		}
		if p.Arguments == nil {
			p.Arguments = map[string]any{}
		}
		for _, req := range t.required {
			if strArg(p.Arguments, req) == "" {
				return nil, &paramError{"缺少必填参数 " + req}
			}
		}
		text, err := t.call(b, p.Arguments)
		if err != nil {
			return nil, err
		}
		return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}, "isError": false}, nil
	}
	return nil, &paramError{"未知工具: " + p.Name}
}

func strArg(a map[string]any, k string) string {
	if v, ok := a[k]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func boolArg(a map[string]any, k string, def bool) bool {
	v, ok := a[k]
	if !ok {
		return def
	}
	b, ok := v.(bool)
	if !ok {
		return def
	}
	return b
}

// intArg 接受 JSON 数字（float64）与字符串两种写法：Agent 常把数字写成字符串。
func intArg(a map[string]any, k string) (int, error) {
	v, ok := a[k]
	if !ok {
		return 0, nil
	}
	switch x := v.(type) {
	case float64:
		return int(x), nil
	case string:
		n, err := strconv.Atoi(x)
		if err != nil {
			return 0, &paramError{k + " 不是整数"}
		}
		return n, nil
	default:
		return 0, &paramError{k + " 类型不对"}
	}
}
