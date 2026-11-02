package mcp

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"runlite/internal/api"
	"runlite/internal/env"
	"runlite/internal/logs"
	"runlite/internal/proc"
	"runlite/internal/project"
	"runlite/internal/service"
)

// fake 只记录"转给 daemon 的是什么"，不真起进程：MCP 面要保证的是协议与参数搬运正确，
// 真启停已经在 /api 那层验过。
type fake struct {
	projects  []api.ProjectView
	startKey  string
	startCfg  string
	startWait bool
	stopKey   string
	stopForce bool
	logsN     int
	logsGrep  string
	addDir    string
	removeKey string
	startErr  error
	statusKey string
}

func (f *fake) Projects() ([]api.ProjectView, error) { return f.projects, nil }

func (f *fake) Project(key string) (api.ProjectView, error) {
	f.statusKey = key
	return api.ProjectView{ID: "abc", Name: "demo", Configs: []project.LaunchConfig{{ID: "spring-boot-run"}}}, nil
}

func (f *fake) Start(key string, req service.StartRequest, wait bool) (*proc.Run, error) {
	f.startKey, f.startCfg, f.startWait = key, req.Config, wait
	if f.startErr != nil {
		return nil, f.startErr
	}
	return &proc.Run{RunInfo: proc.RunInfo{Name: "demo", State: proc.StateRunning, PID: 4321, Port: 8081, LogPath: `D:\tmp\x.log`}}, nil
}

func (f *fake) Stop(key string, force bool) (*proc.Run, error) {
	f.stopKey, f.stopForce = key, force
	return &proc.Run{RunInfo: proc.RunInfo{Name: "demo", State: proc.StateStopped, Reason: "本来就没有在运行", NoOp: true}}, nil
}

func (f *fake) Logs(key string, n, since int, grep string) (*api.LogsResponse, error) {
	f.logsN, f.logsGrep = n, grep
	return &api.LogsResponse{Run: &proc.Run{RunInfo: proc.RunInfo{Name: "demo", State: proc.StateFailed}},
		Lines: []logs.Line{{Text: "BUILD FAILURE"}}}, nil
}

func (f *fake) Add(req service.AddRequest) (api.ProjectView, error) {
	f.addDir = req.Dir
	return api.ProjectView{ID: "new1", Name: "fresh", Dir: req.Dir}, nil
}

func (f *fake) Remove(key string) error { f.removeKey = key; return nil }

func (f *fake) Doctor() (*env.Report, error) { return &env.Report{}, nil }

func call(t *testing.T, f *fake, method string, params map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	resp, reply := handle(raw, f)
	if !reply {
		t.Fatalf("%s 没有回包", method)
	}
	out, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func toolText(t *testing.T, resp map[string]any) string {
	t.Helper()
	res, ok := resp["result"].(map[string]any)
	if !ok {
		t.Fatalf("响应里没有 result: %v", resp)
	}
	arr, ok := res["content"].([]any)
	if !ok || len(arr) == 0 {
		t.Fatalf("响应里没有 content: %v", resp)
	}
	first, ok := arr[0].(map[string]any)
	if !ok {
		t.Fatalf("content[0] 不是对象: %v", arr[0])
	}
	s, _ := first["text"].(string)
	return s
}

func TestInitializeAndToolList(t *testing.T) {
	f := &fake{}
	init := call(t, f, "initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}})
	res, ok := init["result"].(map[string]any)
	if !ok {
		t.Fatalf("initialize 无 result: %v", init)
	}
	if res["protocolVersion"] != "2025-06-18" {
		t.Errorf("protocolVersion = %v, 期望原样回客户端要求的版本", res["protocolVersion"])
	}
	if info, ok := res["serverInfo"].(map[string]any); !ok || info["name"] != "runlite" {
		t.Errorf("serverInfo = %v, 期望 name=runlite", res["serverInfo"])
	}
	if _, ok := res["capabilities"].(map[string]any)["tools"]; !ok {
		t.Errorf("capabilities = %v, 期望声明 tools 能力", res["capabilities"])
	}

	list := call(t, f, "tools/list", nil)
	toolsOut, ok := list["result"].(map[string]any)["tools"].([]any)
	if !ok || len(toolsOut) != len(tools) {
		t.Fatalf("tools 条数 = %d, 期望 %d", len(toolsOut), len(tools))
	}
	want := []string{"add_project", "doctor", "list_projects", "project_status", "read_logs",
		"remove_project", "start_project", "stop_project"}
	for i, name := range want {
		tv := toolsOut[i].(map[string]any)
		if tv["name"] != name {
			t.Errorf("第 %d 个工具 = %v, 期望 %s（列表按名字排序，Agent 靠它稳定寻址）", i, tv["name"], name)
		}
		schema, ok := tv["inputSchema"].(map[string]any)
		if !ok || schema["type"] != "object" {
			t.Errorf("%s 的 inputSchema = %v, 期望 type=object", name, tv["inputSchema"])
		}
		if req, ok := schema["required"].([]any); !ok || (name == "start_project" && len(req) == 0) {
			t.Errorf("%s 的 required = %v, 期望列出必填项", name, schema["required"])
		}
	}
}

func TestStartProjectForwardsArgsAndWaitDefault(t *testing.T) {
	f := &fake{}
	resp := call(t, f, "tools/call", map[string]any{"name": "start_project",
		"arguments": map[string]any{"project": "lgsy", "config": "spring-boot-run"}})
	if f.startKey != "lgsy" || f.startCfg != "spring-boot-run" {
		t.Errorf("转给 daemon 的参数 = %q/%q, 期望 lgsy/spring-boot-run", f.startKey, f.startCfg)
	}
	if !f.startWait {
		t.Error("wait 没给时默认必须等就绪，否则 Agent 拿到 starting 就以为起来了")
	}
	text := toolText(t, resp)
	if !strings.Contains(text, `"state": "running"`) || !strings.Contains(text, `"port": 8081`) {
		t.Errorf("结果文本 = %q, 期望带上 state 与端口", text)
	}
	if resp["error"] != nil {
		t.Errorf("成功调用不应带协议错误: %v", resp["error"])
	}

	// 关掉等待必须真的传到 daemon，不能被默认值吃掉
	call(t, f, "tools/call", map[string]any{"name": "start_project",
		"arguments": map[string]any{"project": "lgsy", "wait": false}})
	if f.startWait {
		t.Error("wait=false 被忽略")
	}
}

// TestDaemonFailureIsToolErrorNotProtocolError 钉住两类错误的分工：
// 项目起不来是工具结果里的 isError（Agent 应该去读日志），参数写错才是协议错误。
// 少了这条区分，Agent 会把"端口被占"当成自己传参有误反复重试。
func TestDaemonFailureIsToolErrorNotProtocolError(t *testing.T) {
	f := &fake{startErr: errors.New("端口 8080 已被 PID 27208 占用")}
	resp := call(t, f, "tools/call", map[string]any{"name": "start_project",
		"arguments": map[string]any{"project": "lgsy"}})
	if resp["error"] != nil {
		t.Fatalf("后端失败被当成协议错误抛了: %v", resp["error"])
	}
	res := resp["result"].(map[string]any)
	if res["isError"] != true {
		t.Errorf("isError = %v, 期望 true", res["isError"])
	}
	if !strings.Contains(toolText(t, resp), "端口 8080 已被 PID 27208 占用") {
		t.Errorf("结果文本没带原始原因: %q", toolText(t, resp))
	}
}

func TestBadAndMissingParamsAreProtocolErrors(t *testing.T) {
	f := &fake{}
	// 缺必填：不能走到后端
	resp := call(t, f, "tools/call", map[string]any{"name": "start_project", "arguments": map[string]any{}})
	errObj, ok := resp["error"].(map[string]any)
	if !ok || errObj["code"] != float64(codeBadParams) {
		t.Fatalf("缺 project 时的响应 = %v, 期望 %d 参数错误", resp, codeBadParams)
	}
	if f.startKey != "" {
		t.Error("参数不全还去调了 daemon")
	}
	// 未知工具同样必须在协议层拒绝，不能静默返回空结果
	resp = call(t, f, "tools/call", map[string]any{"name": "drop_database", "arguments": map[string]any{}})
	if errObj, ok := resp["error"].(map[string]any); !ok || !strings.Contains(errObj["message"].(string), "未知工具") {
		t.Errorf("未知工具的响应 = %v, 期望报未知工具", resp)
	}
	// lines 写成非整数：报参数错误而不是悄悄按默认取
	resp = call(t, f, "tools/call", map[string]any{"name": "read_logs",
		"arguments": map[string]any{"project": "lgsy", "lines": "abc"}})
	if _, ok := resp["error"].(map[string]any); !ok {
		t.Errorf("lines=abc 的响应 = %v, 期望参数错误", resp)
	}
}

// TestNotificationsGetNoReply：通知回包会让客户端把响应错配到上一条请求上，
// 整个会话的 id 从此错位。
func TestNotificationsGetNoReply(t *testing.T) {
	var in strings.Builder
	in.WriteString(`{"jsonrpc":"2.0","method":"notifications/initialized"}` + "\n")
	in.WriteString(`{"jsonrpc":"2.0","id":"req-7","method":"initialize","params":{"protocolVersion":"2024-11-05"}}` + "\n")
	var out strings.Builder
	if err := Serve(strings.NewReader(in.String()), &out, &fake{}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("输出 %d 行: %q, 期望通知不回包、只剩 initialize 的一行", len(lines), out.String())
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &m); err != nil {
		t.Fatalf("第一行不是合法 JSON（%v）：%s", err, lines[0])
	}
	if m["id"] != "req-7" {
		t.Errorf("响应 id = %v, 期望原样回 req-7（客户端靠它配对请求）", m["id"])
	}
	if m["result"] == nil {
		t.Errorf("initialize 响应不完整: %s", lines[0])
	}
}

func TestGarbageLineGetsParseError(t *testing.T) {
	resp, reply := handle([]byte(`{not json`), &fake{})
	if !reply || resp.Error == nil || resp.Error.Code != codeParseError {
		t.Errorf("坏 JSON 的响应 = %+v, 期望 %d", resp, codeParseError)
	}
}

func TestReadLogsAndStopShape(t *testing.T) {
	f := &fake{}
	resp := call(t, f, "tools/call", map[string]any{"name": "read_logs",
		"arguments": map[string]any{"project": "lgsy", "lines": 50, "grep": "ERROR"}})
	if f.logsN != 50 || f.logsGrep != "ERROR" {
		t.Errorf("转给 daemon 的取参 = %d/%q, 期望 50/ERROR", f.logsN, f.logsGrep)
	}
	if !strings.Contains(toolText(t, resp), "BUILD FAILURE") {
		t.Errorf("日志文本缺内容: %q", toolText(t, resp))
	}

	resp = call(t, f, "tools/call", map[string]any{"name": "stop_project",
		"arguments": map[string]any{"project": "lgsy"}})
	if !f.stopForce {
		t.Error("force 默认应为 true")
	}
	if !strings.Contains(toolText(t, resp), `"noOp": true`) {
		t.Errorf("重复 stop 要能让 Agent 看出什么都没做: %q", toolText(t, resp))
	}

	call(t, f, "tools/call", map[string]any{"name": "add_project",
		"arguments": map[string]any{"dir": "D:/tmp/demo"}})
	if f.addDir != "D:/tmp/demo" {
		t.Errorf("add 转发的目录 = %q", f.addDir)
	}
	call(t, f, "tools/call", map[string]any{"name": "remove_project",
		"arguments": map[string]any{"project": "new1"}})
	if f.removeKey != "new1" {
		t.Errorf("remove 转发的键 = %q", f.removeKey)
	}
}

// TestServeSurvivesOversizedLine：一条超过 4MB 的输入行曾经让 Serve 直接返回
// ErrTooLong，整个 MCP 进程退出——一条胖消息就能打死服务。现在超限行被丢弃并
// 回一条解析错误，后面的合法请求照常服务。
func TestServeSurvivesOversizedLine(t *testing.T) {
	fat := strings.Repeat("x", maxLine+1)
	var in strings.Builder
	in.WriteString(`{"jsonrpc":"2.0","id":"req-1","method":"ping"}` + "\n")
	in.WriteString(fat + "\n")
	in.WriteString(`{"jsonrpc":"2.0","id":"req-2","method":"ping"}` + "\n")

	var out strings.Builder
	if err := Serve(strings.NewReader(in.String()), &out, &fake{}); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("输出 %d 行: %q, 期望 ping + 解析错误 + ping", len(lines), out.String())
	}
	var parseErr map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &parseErr); err != nil {
		t.Fatalf("第二行不是合法 JSON（%v）：%s", err, lines[1])
	}
	if code, _ := parseErr["error"].(map[string]any)["code"].(float64); code != codeParseError {
		t.Errorf("超限行响应 = %v, 期望 code=%d 的解析错误", lines[1], codeParseError)
	}
	var last map[string]any
	if err := json.Unmarshal([]byte(lines[2]), &last); err != nil {
		t.Fatalf("第三行不是合法 JSON（%v）：%s", err, lines[2])
	}
	if last["id"] != "req-2" {
		t.Errorf("超限行之后的服务被打断: 第三行 = %s", lines[2])
	}
}
