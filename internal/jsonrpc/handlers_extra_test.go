package jsonrpc

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func dialTCP(addr string) (net.Conn, error) {
	return net.DialTimeout("tcp", addr, 2*time.Second)
}

// --- task.list success ---

func TestHandler_TaskList_Success(t *testing.T) {
	dir := t.TempDir()

	evalContent := `name: task-list-eval
skill: test-skill
config:
  trials_per_task: 1
  timeout_seconds: 30
  executor: codex-cli
  model: test
tasks:
  - "tasks/*.yaml"
`
	evalPath := filepath.Join(dir, "eval.yaml")
	require.NoError(t, os.WriteFile(evalPath, []byte(evalContent), 0644))

	tasksDir := filepath.Join(dir, "tasks")
	require.NoError(t, os.MkdirAll(tasksDir, 0755))

	taskContent := `id: task-alpha
name: Alpha Task
inputs:
  prompt: "Do something"
`
	require.NoError(t, os.WriteFile(filepath.Join(tasksDir, "alpha.yaml"), []byte(taskContent), 0644))

	server := newTestServer()
	resp := rpcCall(t, server, "task.list", map[string]string{"path": evalPath})
	assert.Nil(t, resp.Error)

	data, err := json.Marshal(resp.Result)
	require.NoError(t, err)
	var result TaskListResult
	require.NoError(t, json.Unmarshal(data, &result))
	assert.Len(t, result.Tasks, 1)
	assert.Equal(t, "task-alpha", result.Tasks[0].ID)
	assert.Equal(t, "Alpha Task", result.Tasks[0].Name)
}

func TestHandler_TaskList_MultipleTasks(t *testing.T) {
	dir := t.TempDir()

	evalContent := `name: multi-task-eval
config:
  trials_per_task: 1
  timeout_seconds: 30
  executor: codex-cli
  model: test
tasks:
  - "tasks/*.yaml"
`
	evalPath := filepath.Join(dir, "eval.yaml")
	require.NoError(t, os.WriteFile(evalPath, []byte(evalContent), 0644))

	tasksDir := filepath.Join(dir, "tasks")
	require.NoError(t, os.MkdirAll(tasksDir, 0755))

	for i, name := range []string{"alpha", "beta", "gamma"} {
		upper := strings.ToUpper(name[:1]) + name[1:]
		content := fmt.Sprintf("id: task-%s\nname: %s Task\ninputs:\n  prompt: \"task %d\"\n", name, upper, i)
		require.NoError(t, os.WriteFile(filepath.Join(tasksDir, name+".yaml"), []byte(content), 0644))
	}

	server := newTestServer()
	resp := rpcCall(t, server, "task.list", map[string]string{"path": evalPath})
	assert.Nil(t, resp.Error)

	data, err := json.Marshal(resp.Result)
	require.NoError(t, err)
	var result TaskListResult
	require.NoError(t, json.Unmarshal(data, &result))
	assert.Len(t, result.Tasks, 3)
}

func TestHandler_TaskList_MissingPath(t *testing.T) {
	server := newTestServer()
	resp := rpcCall(t, server, "task.list", map[string]string{"path": ""})
	require.NotNil(t, resp.Error)
	assert.Equal(t, CodeInvalidParams, resp.Error.Code)
}

func TestHandler_TaskList_InvalidParams(t *testing.T) {
	server := newTestServer()
	resp := rpcCall(t, server, "task.list", "not an object")
	require.NotNil(t, resp.Error)
	assert.Equal(t, CodeInvalidParams, resp.Error.Code)
}

// --- task.get success ---

func TestHandler_TaskGet_Success(t *testing.T) {
	dir := t.TempDir()

	evalContent := `name: task-get-eval
config:
  trials_per_task: 1
  timeout_seconds: 30
  executor: codex-cli
  model: test
tasks:
  - "tasks/*.yaml"
`
	evalPath := filepath.Join(dir, "eval.yaml")
	require.NoError(t, os.WriteFile(evalPath, []byte(evalContent), 0644))

	tasksDir := filepath.Join(dir, "tasks")
	require.NoError(t, os.MkdirAll(tasksDir, 0755))

	taskContent := `id: get-this-task
name: Get This Task
inputs:
  prompt: "hello world"
`
	require.NoError(t, os.WriteFile(filepath.Join(tasksDir, "task1.yaml"), []byte(taskContent), 0644))

	server := newTestServer()
	resp := rpcCall(t, server, "task.get", map[string]string{
		"path":    evalPath,
		"task_id": "get-this-task",
	})
	assert.Nil(t, resp.Error)
	require.NotNil(t, resp.Result)

	data, err := json.Marshal(resp.Result)
	require.NoError(t, err)
	// Result is a TestCase, check it has our task ID
	assert.Contains(t, string(data), "get-this-task")
}

func TestHandler_TaskGet_NotFoundTask(t *testing.T) {
	dir := t.TempDir()

	evalContent := `name: task-get-eval
config:
  trials_per_task: 1
  timeout_seconds: 30
  executor: codex-cli
  model: test
tasks:
  - "tasks/*.yaml"
`
	evalPath := filepath.Join(dir, "eval.yaml")
	require.NoError(t, os.WriteFile(evalPath, []byte(evalContent), 0644))

	tasksDir := filepath.Join(dir, "tasks")
	require.NoError(t, os.MkdirAll(tasksDir, 0755))

	taskContent := `id: existing-task
name: Existing Task
inputs:
  prompt: "hello"
`
	require.NoError(t, os.WriteFile(filepath.Join(tasksDir, "task.yaml"), []byte(taskContent), 0644))

	server := newTestServer()
	resp := rpcCall(t, server, "task.get", map[string]string{
		"path":    evalPath,
		"task_id": "nonexistent-task-id",
	})
	require.NotNil(t, resp.Error)
	assert.Equal(t, CodeInvalidParams, resp.Error.Code)
}

func TestHandler_TaskGet_MissingTaskID(t *testing.T) {
	dir := t.TempDir()

	evalContent := `name: task-get-eval
config:
  trials_per_task: 1
  timeout_seconds: 30
  executor: codex-cli
  model: test
tasks:
  - "tasks/*.yaml"
`
	evalPath := filepath.Join(dir, "eval.yaml")
	require.NoError(t, os.WriteFile(evalPath, []byte(evalContent), 0644))

	server := newTestServer()
	resp := rpcCall(t, server, "task.get", map[string]string{
		"path":    evalPath,
		"task_id": "",
	})
	require.NotNil(t, resp.Error)
	assert.Equal(t, CodeInvalidParams, resp.Error.Code)
}

func TestHandler_TaskGet_EvalNotFound(t *testing.T) {
	server := newTestServer()
	resp := rpcCall(t, server, "task.get", map[string]string{
		"path":    "/nonexistent/eval.yaml",
		"task_id": "some-task",
	})
	require.NotNil(t, resp.Error)
	assert.Equal(t, CodeEvalNotFound, resp.Error.Code)
}

// --- run.cancel ---

func TestHandler_RunCancel_Success(t *testing.T) {
	dir := t.TempDir()
	evalContent := `name: cancel-eval
config:
  trials_per_task: 1
  timeout_seconds: 30
  executor: codex-cli
  model: test
tasks:
  - "tasks/*.yaml"
`
	evalPath := filepath.Join(dir, "eval.yaml")
	require.NoError(t, os.WriteFile(evalPath, []byte(evalContent), 0644))

	registry := NewMethodRegistry()
	hctx := NewHandlerContext()
	RegisterHandlers(registry, hctx)
	server := NewServer(registry, nil)

	// Start a run — inject a "running" state manually to avoid timing issues
	hctx.mu.Lock()
	hctx.nextRunID++
	runID := fmt.Sprintf("run-%d", hctx.nextRunID)
	hctx.runs[runID] = &RunState{ID: runID, Status: "running"}
	_, cancel := createTestCancel()
	hctx.cancelFuncs[runID] = cancel
	hctx.mu.Unlock()

	// Cancel it
	resp := rpcCall(t, server, "run.cancel", map[string]string{"run_id": runID})
	assert.Nil(t, resp.Error)

	data, err := json.Marshal(resp.Result)
	require.NoError(t, err)
	var result RunCancelResult
	require.NoError(t, json.Unmarshal(data, &result))
	assert.True(t, result.Canceled)

	// Verify status changed
	hctx.mu.Lock()
	state := hctx.runs[runID]
	_, cancelExists := hctx.cancelFuncs[runID]
	hctx.mu.Unlock()
	assert.Equal(t, "canceled", state.Status)
	assert.False(t, cancelExists, "cancel func should be cleaned up")
}

func TestHandler_RunCancel_AlreadyCompleted(t *testing.T) {
	registry := NewMethodRegistry()
	hctx := NewHandlerContext()
	RegisterHandlers(registry, hctx)
	server := NewServer(registry, nil)

	// Manually inject a completed run
	hctx.mu.Lock()
	hctx.runs["run-done"] = &RunState{ID: "run-done", Status: "completed"}
	hctx.mu.Unlock()

	resp := rpcCall(t, server, "run.cancel", map[string]string{"run_id": "run-done"})
	assert.Nil(t, resp.Error)

	data, err := json.Marshal(resp.Result)
	require.NoError(t, err)
	var result RunCancelResult
	require.NoError(t, json.Unmarshal(data, &result))
	assert.False(t, result.Canceled, "cannot cancel a completed run")
}

func TestHandler_RunCancel_MissingRunID(t *testing.T) {
	server := newTestServer()
	resp := rpcCall(t, server, "run.cancel", map[string]string{"run_id": ""})
	require.NotNil(t, resp.Error)
	assert.Equal(t, CodeInvalidParams, resp.Error.Code)
}

func TestHandler_RunCancel_InvalidParams(t *testing.T) {
	server := newTestServer()
	resp := rpcCall(t, server, "run.cancel", "not json")
	require.NotNil(t, resp.Error)
	assert.Equal(t, CodeInvalidParams, resp.Error.Code)
}

// --- eval.list edge cases ---

func TestHandler_EvalList_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	server := newTestServer()
	resp := rpcCall(t, server, "eval.list", map[string]string{"dir": dir})
	assert.Nil(t, resp.Error)

	data, err := json.Marshal(resp.Result)
	require.NoError(t, err)
	var result EvalListResult
	require.NoError(t, json.Unmarshal(data, &result))
	assert.Nil(t, result.Evals)
}

// --- eval.get edge cases ---

func TestHandler_EvalGet_MissingPath(t *testing.T) {
	server := newTestServer()
	resp := rpcCall(t, server, "eval.get", map[string]string{"path": ""})
	require.NotNil(t, resp.Error)
	assert.Equal(t, CodeInvalidParams, resp.Error.Code)
}

func TestHandler_EvalGet_InvalidParams(t *testing.T) {
	server := newTestServer()
	resp := rpcCall(t, server, "eval.get", "not an object")
	require.NotNil(t, resp.Error)
	assert.Equal(t, CodeInvalidParams, resp.Error.Code)
}

// --- eval.validate edge cases ---

func TestHandler_EvalValidate_MissingPath(t *testing.T) {
	server := newTestServer()
	resp := rpcCall(t, server, "eval.validate", map[string]string{"path": ""})
	require.NotNil(t, resp.Error)
	assert.Equal(t, CodeInvalidParams, resp.Error.Code)
}

func TestHandler_EvalValidate_NotFound(t *testing.T) {
	server := newTestServer()
	resp := rpcCall(t, server, "eval.validate", map[string]string{"path": "/nonexistent/eval.yaml"})
	require.NotNil(t, resp.Error)
	assert.Equal(t, CodeEvalNotFound, resp.Error.Code)
}

func TestHandler_EvalValidate_MalformedYAML(t *testing.T) {
	dir := t.TempDir()
	evalPath := filepath.Join(dir, "eval.yaml")
	require.NoError(t, os.WriteFile(evalPath, []byte("not: [valid: yaml: ::::"), 0644))

	server := newTestServer()
	resp := rpcCall(t, server, "eval.validate", map[string]string{"path": evalPath})
	assert.Nil(t, resp.Error)

	data, err := json.Marshal(resp.Result)
	require.NoError(t, err)
	var result EvalValidateResult
	require.NoError(t, json.Unmarshal(data, &result))
	assert.False(t, result.Valid)
	assert.NotEmpty(t, result.Errors)
	// Should contain YAML syntax error
	found := false
	for _, e := range result.Errors {
		if strings.Contains(e, "YAML syntax") {
			found = true
			break
		}
	}
	assert.True(t, found, "expected YAML syntax error, got: %v", result.Errors)
}

// --- eval.run edge cases ---

func TestHandler_EvalRun_MissingPath(t *testing.T) {
	server := newTestServer()
	resp := rpcCall(t, server, "eval.run", map[string]string{"path": ""})
	require.NotNil(t, resp.Error)
	assert.Equal(t, CodeInvalidParams, resp.Error.Code)
}

func TestHandler_EvalRun_InvalidParams(t *testing.T) {
	server := newTestServer()
	resp := rpcCall(t, server, "eval.run", "not json")
	require.NotNil(t, resp.Error)
	assert.Equal(t, CodeInvalidParams, resp.Error.Code)
}

// --- run.status edge cases ---

func TestHandler_RunStatus_MissingRunID(t *testing.T) {
	server := newTestServer()
	resp := rpcCall(t, server, "run.status", map[string]string{"run_id": ""})
	require.NotNil(t, resp.Error)
	assert.Equal(t, CodeInvalidParams, resp.Error.Code)
}

func TestHandler_RunStatus_InvalidParams(t *testing.T) {
	server := newTestServer()
	resp := rpcCall(t, server, "run.status", "bad params")
	require.NotNil(t, resp.Error)
	assert.Equal(t, CodeInvalidParams, resp.Error.Code)
}

// --- TCP transport ---

func TestTCPListener_StartAndClose(t *testing.T) {
	registry := NewMethodRegistry()
	registry.Register("ping", func(_ context.Context, _ json.RawMessage) (any, *Error) {
		return map[string]string{"pong": "ok"}, nil
	})
	server := NewServer(registry, nil)

	listener, err := NewTCPListener("127.0.0.1:0", server)
	require.NoError(t, err)
	require.NotNil(t, listener)
	require.NotNil(t, listener.Addr())

	// Close should work without error
	require.NoError(t, listener.Close())
}

func TestTCPListener_InvalidAddress(t *testing.T) {
	registry := NewMethodRegistry()
	server := NewServer(registry, nil)

	_, err := NewTCPListener("invalid:address:too:many:colons:-1", server)
	assert.Error(t, err)
}

func TestTCPListener_ServeAndQuery(t *testing.T) {
	registry := NewMethodRegistry()
	registry.Register("ping", func(_ context.Context, _ json.RawMessage) (any, *Error) {
		return map[string]string{"status": "ok"}, nil
	})
	server := NewServer(registry, nil)

	listener, err := NewTCPListener("127.0.0.1:0", server)
	require.NoError(t, err)

	// Serve in background
	go func() {
		_ = listener.Serve()
	}()
	defer listener.Close() //nolint:errcheck

	// Connect and send a request — no sleep needed, listener is already active
	addr := listener.Addr().String()
	conn, err := dialTCP(addr)
	require.NoError(t, err)
	defer conn.Close() //nolint:errcheck

	reqLine := `{"jsonrpc":"2.0","method":"ping","params":{},"id":1}` + "\n"
	_, err = conn.Write([]byte(reqLine))
	require.NoError(t, err)

	// Read response line (newline-delimited JSON)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	scanner := bufio.NewScanner(conn)
	require.True(t, scanner.Scan(), "expected response line")
	require.NoError(t, scanner.Err())

	var resp Response
	require.NoError(t, json.Unmarshal(scanner.Bytes(), &resp))
	assert.Nil(t, resp.Error)
	assert.Equal(t, "2.0", resp.JSONRPC)
}

func createTestCancel() (context.Context, context.CancelFunc) {
	return context.WithCancel(context.Background())
}
