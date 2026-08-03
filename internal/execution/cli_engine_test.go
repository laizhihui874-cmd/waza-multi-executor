package execution

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/microsoft/waza/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCLIEngineHelperProcess(t *testing.T) {
	if os.Getenv("WAZA_CLI_HELPER") != "1" {
		return
	}
	prompt, _ := io.ReadAll(os.Stdin)
	mode := os.Getenv("WAZA_CLI_HELPER_MODE")
	switch mode {
	case "error":
		fmt.Fprintln(os.Stderr, "helper failed safely")
		os.Exit(7)
	case "jsonl":
		fmt.Println(`{"type":"thread.started","thread_id":"thread-1"}`)
		fmt.Printf("{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":%q}}\n", "answer: "+string(prompt))
		fmt.Println(`{"type":"turn.completed","usage":{"input_tokens":11,"cached_input_tokens":3,"output_tokens":5}}`)
	case "skill":
		fmt.Printf("skill=%s", os.Getenv("WAZA_SKILL_DIR"))
	case "sleep":
		time.Sleep(10 * time.Second)
		fmt.Print("too late")
	case "large-error":
		fmt.Fprint(os.Stderr, strings.Repeat("x", defaultStderrLimit+1024))
		os.Exit(9)
	case "unauthenticated":
		fmt.Fprintln(os.Stderr, "authentication required: run agent login")
		os.Exit(1)
	case "claude-auth-error":
		fmt.Println(`{"type":"result","is_error":true,"result":"Not logged in - Please run /login"}`)
		os.Exit(1)
	case "spawn-child":
		child := exec.Command("sleep", "10")
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(11)
		}
		workspace := os.Getenv("WAZA_WORKSPACE")
		_ = os.WriteFile(filepath.Join(workspace, "child.pid"), []byte(fmt.Sprint(child.Process.Pid)), 0o600)
		time.Sleep(10 * time.Second)
	default:
		workspace := os.Getenv("WAZA_WORKSPACE")
		_ = os.WriteFile(filepath.Join(workspace, "agent-output.txt"), []byte("created"), 0o600)
		fmt.Printf("answer: %s", prompt)
	}
	os.Exit(0)
}

func helperExecutorConfig(t *testing.T, mode string) models.ExecutorConfig {
	t.Helper()
	t.Setenv("WAZA_CLI_HELPER", "1")
	t.Setenv("WAZA_CLI_HELPER_MODE", mode)
	return models.ExecutorConfig{
		Command:         os.Args[0],
		Args:            []string{"-test.run=TestCLIEngineHelperProcess"},
		EnvAllowlist:    []string{"WAZA_CLI_HELPER", "WAZA_CLI_HELPER_MODE"},
		PromptTransport: "stdin",
		OutputFormat:    "text",
	}
}

func TestGenericCLIEngineExecutesWithoutShellAndCapturesWorkspace(t *testing.T) {
	engine := NewGenericCLIEngine("test-model", helperExecutorConfig(t, "text"))
	require.NoError(t, engine.Initialize(context.Background()))
	t.Cleanup(func() { require.NoError(t, engine.Shutdown(context.Background())) })

	resp, err := engine.Execute(context.Background(), &ExecutionRequest{
		Message:   "hello",
		Resources: []ResourceFile{{Path: "input.txt", Content: []byte("fixture")}},
	})
	require.NoError(t, err)
	assert.True(t, resp.Success)
	assert.Equal(t, "answer: hello", resp.FinalOutput)
	assert.Equal(t, []byte("fixture"), resp.WorkspaceFiles["input.txt"])
	assert.Equal(t, []byte("created"), resp.WorkspaceFiles["agent-output.txt"])
}

func TestGenericCLIEngineReportsExitCodeAndStderr(t *testing.T) {
	engine := NewGenericCLIEngine("test-model", helperExecutorConfig(t, "error"))
	require.NoError(t, engine.Initialize(context.Background()))
	t.Cleanup(func() { require.NoError(t, engine.Shutdown(context.Background())) })

	resp, err := engine.Execute(context.Background(), &ExecutionRequest{Message: "hello"})
	require.Error(t, err)
	require.NotNil(t, resp)
	assert.False(t, resp.Success)
	assert.Contains(t, err.Error(), "exit code 7")
	assert.Contains(t, err.Error(), "helper failed safely")
}

func TestGenericCLIEngineInitializeRejectsMissingCommand(t *testing.T) {
	engine := NewGenericCLIEngine("test-model", models.ExecutorConfig{Command: filepath.Join(t.TempDir(), "missing")})
	err := engine.Initialize(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "executable")
}

func TestGenericCLIEngineReportsAuthenticationFailure(t *testing.T) {
	engine := NewGenericCLIEngine("test-model", helperExecutorConfig(t, "unauthenticated"))
	require.NoError(t, engine.Initialize(context.Background()))
	t.Cleanup(func() { require.NoError(t, engine.Shutdown(context.Background())) })

	_, err := engine.Execute(context.Background(), &ExecutionRequest{Message: "hello"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "authentication required")
}

func TestGenericCLIEngineRejectsResourcePathEscape(t *testing.T) {
	engine := NewGenericCLIEngine("test-model", helperExecutorConfig(t, "text"))
	require.NoError(t, engine.Initialize(context.Background()))
	t.Cleanup(func() { require.NoError(t, engine.Shutdown(context.Background())) })

	_, err := engine.Execute(context.Background(), &ExecutionRequest{
		Message: "hello", Resources: []ResourceFile{{Path: "../escape.txt", Content: []byte("no")}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "workspace")
}

func TestCodexCLIEngineParsesJSONLUsage(t *testing.T) {
	cfg := helperExecutorConfig(t, "jsonl")
	cfg.OutputFormat = "codex-jsonl"
	engine := newCLIEngine("codex-cli", "test-model", cfg, cliAdapter{PromptTransport: "stdin"})
	require.NoError(t, engine.Initialize(context.Background()))
	t.Cleanup(func() { require.NoError(t, engine.Shutdown(context.Background())) })

	resp, err := engine.Execute(context.Background(), &ExecutionRequest{Message: "hello"})
	require.NoError(t, err)
	assert.Equal(t, "answer: hello", resp.FinalOutput)
	require.NotNil(t, resp.Usage)
	assert.Equal(t, 11, resp.Usage.InputTokens)
	assert.Equal(t, 3, resp.Usage.CacheReadTokens)
	assert.Equal(t, 5, resp.Usage.OutputTokens)
	assert.Equal(t, "thread-1", resp.SessionID)
	usage := engine.SessionUsage(resp.SessionID)
	require.NotNil(t, usage)
	assert.Equal(t, 11, usage.InputTokens)
}

func TestParseCodexJSONLRejectsMalformedEvent(t *testing.T) {
	_, err := parseCodexJSONL(strings.NewReader("not-json\n"), "test-model")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "codex JSONL")
}

func TestParseCodexJSONLCollectsCommandAndMCPEvents(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"item.completed","item":{"id":"cmd-1","type":"command_execution","command":"pwd","aggregated_output":"/tmp","exit_code":0}}`,
		`{"type":"item.completed","item":{"id":"cmd-2","type":"command_execution","command":"false","aggregated_output":"","exit_code":2}}`,
		`{"type":"item.completed","item":{"id":"mcp-1","type":"mcp_tool_call","server":"files","tool":"read","arguments":{"path":"a"},"result":{"text":"ok"}}}`,
		`{"type":"item.completed","item":{"id":"mcp-2","type":"mcp_tool_call","server":"files","tool":"write","arguments":{},"error":"denied"}}`,
		`{"type":"turn.failed","error":{"message":"turn broke"}}`,
	}, "\n")
	result, err := parseCodexJSONL(strings.NewReader(input), "test-model")
	require.NoError(t, err)
	require.Len(t, result.ToolEvents, 4)
	assert.True(t, result.ToolEvents[0].Success)
	assert.False(t, result.ToolEvents[1].Success)
	assert.Equal(t, "files/read", result.ToolEvents[2].ToolName)
	assert.False(t, result.ToolEvents[3].Success)
	assert.Equal(t, "turn broke", result.ErrorMsg)
}

func TestGenericCLIEngineMaterializesOnlyTargetSkill(t *testing.T) {
	skillsRoot := t.TempDir()
	targetDir := filepath.Join(skillsRoot, "target-skill")
	require.NoError(t, os.MkdirAll(targetDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(targetDir, "SKILL.md"), []byte("---\nname: target-skill\ndescription: test\n---\nbody"), 0o600))

	engine := NewGenericCLIEngine("test-model", helperExecutorConfig(t, "skill"))
	require.NoError(t, engine.Initialize(context.Background()))
	t.Cleanup(func() { require.NoError(t, engine.Shutdown(context.Background())) })
	resp, err := engine.Execute(context.Background(), &ExecutionRequest{
		Message: "hello", SkillName: "target-skill", SkillPaths: []string{skillsRoot},
	})
	require.NoError(t, err)
	assert.Contains(t, resp.FinalOutput, filepath.Join(".waza", "skills", "target-skill"))
	for path := range resp.WorkspaceFiles {
		assert.False(t, strings.HasPrefix(path, ".waza/skills/"), "materialized skill leaked into grader workspace: %s", path)
	}
}

func TestCLIEngineSkillAndBaselineUseMatchingResourceWorkspaces(t *testing.T) {
	skillsRoot := t.TempDir()
	targetDir := filepath.Join(skillsRoot, "target-skill")
	require.NoError(t, os.MkdirAll(targetDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(targetDir, "SKILL.md"), []byte("---\nname: target-skill\ndescription: test\n---\nbody"), 0o600))

	engine := NewGenericCLIEngine("test-model", helperExecutorConfig(t, "skill"))
	require.NoError(t, engine.Initialize(context.Background()))
	t.Cleanup(func() { require.NoError(t, engine.Shutdown(context.Background())) })
	request := ExecutionRequest{
		Message: "hello", SkillName: "target-skill", SkillPaths: []string{skillsRoot},
		Resources: []ResourceFile{{Path: "input.txt", Content: []byte("same fixture")}},
	}

	withSkill, err := engine.Execute(context.Background(), &request)
	require.NoError(t, err)
	request.NoSkills = true
	baseline, err := engine.Execute(context.Background(), &request)
	require.NoError(t, err)

	assert.NotEqual(t, "skill=", withSkill.FinalOutput)
	assert.Equal(t, "skill=", baseline.FinalOutput)
	assert.Equal(t, withSkill.WorkspaceFiles, baseline.WorkspaceFiles)
	assert.Equal(t, []byte("same fixture"), baseline.WorkspaceFiles["input.txt"])
}

func TestCLIEngineMaterializesNativeInstructionFile(t *testing.T) {
	cfg := helperExecutorConfig(t, "text")
	engine := newCLIEngine("codex-cli", "test-model", cfg, cliAdapter{PromptTransport: "stdin"})
	require.NoError(t, engine.Initialize(context.Background()))
	t.Cleanup(func() { require.NoError(t, engine.Shutdown(context.Background())) })

	resp, err := engine.Execute(context.Background(), &ExecutionRequest{
		Message:      "hello",
		Instructions: []InstructionFile{{Path: "rules.md", Content: []byte("follow the rules")}},
	})
	require.NoError(t, err)
	assert.Contains(t, string(resp.WorkspaceFiles["AGENTS.md"]), "follow the rules")
	assert.Equal(t, []string{"CLAUDE.md"}, instructionFilenames("claude-cli"))
	assert.Nil(t, instructionFilenames("generic-cli"))
}

func TestCLIEngineRejectsSymlinkInsideSkill(t *testing.T) {
	skillsRoot := t.TempDir()
	targetDir := filepath.Join(skillsRoot, "target-skill")
	require.NoError(t, os.MkdirAll(targetDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(targetDir, "SKILL.md"), []byte("---\nname: target-skill\n---\n"), 0o600))
	require.NoError(t, os.Symlink(filepath.Join(targetDir, "SKILL.md"), filepath.Join(targetDir, "linked.md")))
	engine := NewGenericCLIEngine("test-model", helperExecutorConfig(t, "text"))
	require.NoError(t, engine.Initialize(context.Background()))
	t.Cleanup(func() { require.NoError(t, engine.Shutdown(context.Background())) })

	_, err := engine.Execute(context.Background(), &ExecutionRequest{
		Message: "hello", SkillName: "target-skill", SkillPaths: []string{skillsRoot},
	})
	require.ErrorContains(t, err, "symbolic link")
}

func TestCLIEngineRejectsSameNamedGlobalSkill(t *testing.T) {
	globalSkill := t.TempDir()
	skillsRoot := t.TempDir()
	targetDir := filepath.Join(skillsRoot, "target-skill")
	require.NoError(t, os.MkdirAll(targetDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(targetDir, "SKILL.md"), []byte("---\nname: target-skill\n---\n"), 0o600))
	cfg := helperExecutorConfig(t, "text")
	engine := newCLIEngine("codex-cli", "test-model", cfg, cliAdapter{
		PromptTransport:   "stdin",
		KnownGlobalSkills: func(string) []string { return []string{globalSkill} },
	})
	require.NoError(t, engine.Initialize(context.Background()))
	t.Cleanup(func() { require.NoError(t, engine.Shutdown(context.Background())) })

	_, err := engine.Execute(context.Background(), &ExecutionRequest{
		Message: "hello", SkillName: "target-skill", SkillPaths: []string{skillsRoot},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "baseline would be contaminated")
}

func TestCLIEngineCancellationTerminatesRun(t *testing.T) {
	engine := NewGenericCLIEngine("test-model", helperExecutorConfig(t, "sleep"))
	require.NoError(t, engine.Initialize(context.Background()))
	t.Cleanup(func() { require.NoError(t, engine.Shutdown(context.Background())) })
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := engine.Execute(ctx, &ExecutionRequest{Message: "hello"})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), 3*time.Second)
}

func TestCLIEngineTruncatesLargeStderr(t *testing.T) {
	engine := NewGenericCLIEngine("test-model", helperExecutorConfig(t, "large-error"))
	require.NoError(t, engine.Initialize(context.Background()))
	t.Cleanup(func() { require.NoError(t, engine.Shutdown(context.Background())) })
	_, err := engine.Execute(context.Background(), &ExecutionRequest{Message: "hello"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "[output truncated by waza]")
}

func TestGenericCLIArgumentTransportExpandsPlaceholdersWithoutShell(t *testing.T) {
	engine := newCLIEngine("generic-cli", "model", models.ExecutorConfig{
		Args: []string{"--workspace={workspace}", "{prompt}"}, PromptTransport: "argument",
	}, cliAdapter{})
	args, err := engine.buildArgs(cliBuildContext{Workspace: "/tmp/work", Prompt: "$(touch should-not-run)"})
	require.NoError(t, err)
	assert.Equal(t, []string{"--workspace=/tmp/work", "$(touch should-not-run)"}, args)

	engine.config.Args = []string{"--prompt-mode"}
	args, err = engine.buildArgs(cliBuildContext{Prompt: ""})
	require.NoError(t, err)
	assert.Equal(t, []string{"--prompt-mode", ""}, args)

	engine.config.Args = []string{"prefix-hello"}
	args, err = engine.buildArgs(cliBuildContext{Prompt: "hello"})
	require.NoError(t, err)
	assert.Equal(t, []string{"prefix-hello", "hello"}, args)

	engine.config.Args = []string{"{prompt}"}
	engine.config.PromptTransport = "stdin"
	_, err = engine.buildArgs(cliBuildContext{Prompt: "hello"})
	require.ErrorContains(t, err, "prompt_transport")
}

func TestCLIEngineConfigValidationRejectsInvalidProtocol(t *testing.T) {
	engine := newCLIEngine("generic-cli", "model", models.ExecutorConfig{PromptTransport: "file"}, cliAdapter{})
	require.ErrorContains(t, engine.validateConfig(), "prompt_transport")
	engine.config.PromptTransport = "stdin"
	engine.config.OutputFormat = "xml"
	require.ErrorContains(t, engine.validateConfig(), "output_format")
}

func TestParseClaudeJSONLUsesResultAndUsage(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"future_event","new_field":{"nested":true}}`,
		`{"type":"assistant","session_id":"session-1","message":{"model":"claude-test","content":[{"type":"text","text":"draft"}],"usage":{"input_tokens":7,"output_tokens":2}}}`,
		`{"type":"result","session_id":"session-1","result":"final","usage":{"input_tokens":8,"output_tokens":3,"cache_read_input_tokens":4}}`,
	}, "\n")
	result, err := parseClaudeJSONL(strings.NewReader(input), "fallback")
	require.NoError(t, err)
	assert.Equal(t, "final", result.FinalOutput)
	assert.Equal(t, "session-1", result.SessionID)
	assert.Equal(t, "claude-test", result.ModelID)
	require.NotNil(t, result.Usage)
	assert.Equal(t, 8, result.Usage.InputTokens)
	assert.Equal(t, 4, result.Usage.CacheReadTokens)
}

func TestParseClaudeJSONLSurfacesAuthenticationFailure(t *testing.T) {
	input := `{"type":"result","is_error":true,"result":"Not logged in - Please run /login","usage":{"input_tokens":0,"output_tokens":0}}`
	result, err := parseClaudeJSONL(strings.NewReader(input), "fallback")
	require.NoError(t, err)
	assert.Equal(t, "Not logged in - Please run /login", result.ErrorMsg)
}

func TestCLIEngineUsesParsedErrorWhenProcessStderrIsEmpty(t *testing.T) {
	cfg := helperExecutorConfig(t, "claude-auth-error")
	cfg.OutputFormat = "claude-jsonl"
	engine := newCLIEngine("claude-cli", "test-model", cfg, cliAdapter{PromptTransport: "stdin"})
	require.NoError(t, engine.Initialize(context.Background()))
	t.Cleanup(func() { require.NoError(t, engine.Shutdown(context.Background())) })

	_, err := engine.Execute(context.Background(), &ExecutionRequest{Message: "hello"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Not logged in")
}

func TestBuiltInCLIAdaptersUseNativeOneShotProtocols(t *testing.T) {
	ctx := cliBuildContext{ModelID: "test-model", Prompt: "hello", WorkDir: t.TempDir(), UsageFile: filepath.Join(t.TempDir(), "usage.json")}

	codexArgs, err := codexCLIAdapter().BuildArgs(ctx)
	require.NoError(t, err)
	assert.Equal(t, "exec", codexArgs[0])
	assert.Contains(t, codexArgs, "--json")
	assert.Contains(t, codexArgs, "--ephemeral")
	assert.Contains(t, codexArgs, "--skip-git-repo-check")

	claudeArgs, err := claudeCLIAdapter().BuildArgs(ctx)
	require.NoError(t, err)
	assert.Contains(t, claudeArgs, "--print")
	assert.Contains(t, claudeArgs, "stream-json")
	assert.Contains(t, claudeArgs, "--no-session-persistence")
	assert.Contains(t, claudeArgs, "--verbose")
}

func TestParseHermesUsageFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"input_tokens":10,"output_tokens":4,"cache_read_tokens":2,"api_calls":3,"model":"hermes-model","provider":"openrouter","session_id":"h-1","failed":false}`), 0o600))
	result, err := parseHermesUsageFile(path, "fallback")
	require.NoError(t, err)
	assert.Equal(t, "h-1", result.SessionID)
	assert.Equal(t, "hermes-model", result.ModelID)
	require.NotNil(t, result.Usage)
	assert.Equal(t, 3, result.Usage.Turns)
	assert.Equal(t, 10, result.Usage.InputTokens)
}

func TestParseHermesUsageFileHandlesMissingFailureAndMalformedReports(t *testing.T) {
	missing, err := parseHermesUsageFile(filepath.Join(t.TempDir(), "missing.json"), "fallback")
	require.NoError(t, err)
	assert.Equal(t, "fallback", missing.ModelID)

	failedPath := filepath.Join(t.TempDir(), "failed.json")
	require.NoError(t, os.WriteFile(failedPath, []byte(`{"failed":true,"failure":"provider rejected request"}`), 0o600))
	failed, err := parseHermesUsageFile(failedPath, "fallback")
	require.NoError(t, err)
	assert.Equal(t, "provider rejected request", failed.ErrorMsg)

	malformedPath := filepath.Join(t.TempDir(), "bad.json")
	require.NoError(t, os.WriteFile(malformedPath, []byte(`not-json`), 0o600))
	_, err = parseHermesUsageFile(malformedPath, "fallback")
	require.ErrorContains(t, err, "parse Hermes usage file")
}

func TestHermesAdapterUsesIsolatedExternalSkillDirectory(t *testing.T) {
	workspace := t.TempDir()
	skillDir := filepath.Join(workspace, ".waza", "hermes-skills", "target-skill")
	require.NoError(t, os.MkdirAll(skillDir, 0o755))
	ctx := cliBuildContext{
		Workspace: workspace,
		WorkDir:   workspace,
		SkillDir:  skillDir,
		UsageFile: filepath.Join(workspace, ".waza", "usage.json"),
		Prompt:    "hello",
	}
	adapter := hermesCLIAdapter()
	require.NoError(t, adapter.PrepareRuntime(ctx))

	data, err := os.ReadFile(filepath.Join(workspace, ".waza", "hermes-home", "config.yaml"))
	require.NoError(t, err)
	assert.Contains(t, string(data), filepath.Join(workspace, ".waza", "hermes-skills"))

	args, err := adapter.BuildArgs(ctx)
	require.NoError(t, err)
	assert.Contains(t, args, "--skills")
	assert.Contains(t, args, "target-skill")
	assert.NotContains(t, args, "--safe-mode")
}

func TestHermesAdapterRejectsSafeModeForSkillEvaluation(t *testing.T) {
	safeMode := true
	_, err := hermesCLIAdapter().BuildArgs(cliBuildContext{
		Config:   models.ExecutorConfig{SafeMode: &safeMode},
		SkillDir: filepath.Join(t.TempDir(), "target-skill"),
	})
	require.ErrorContains(t, err, "safe_mode disables skills")
}

func TestHermesEnvironmentUsesTemporaryHomeAndLoadsCredentials(t *testing.T) {
	sourceHome := t.TempDir()
	t.Setenv("HERMES_HOME", sourceHome)
	require.NoError(t, os.WriteFile(filepath.Join(sourceHome, ".env"), []byte("TEST_HERMES_TOKEN='secret-value'\n"), 0o600))
	workspace := t.TempDir()
	ctx := cliBuildContext{Workspace: workspace}

	env := buildHermesEnvironment(ctx, []string{"PATH=/bin"})
	assert.Contains(t, env, "TEST_HERMES_TOKEN=secret-value")
	assert.Contains(t, env, "HERMES_HOME="+filepath.Join(workspace, ".waza", "hermes-home"))
	assert.Contains(t, env, "HERMES_PLATFORM=cli")
}

func TestReadDotEnvRejectsInvalidNamesAndPreservesExplicitValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	require.NoError(t, os.WriteFile(path, []byte("GOOD_NAME=ok\n1BAD=no\nexport ALSO_GOOD=two\n"), 0o600))
	assert.ElementsMatch(t, []string{"GOOD_NAME=ok", "ALSO_GOOD=two"}, readDotEnv(path))

	ctx := cliBuildContext{Workspace: t.TempDir()}
	t.Setenv("HERMES_HOME", filepath.Dir(path))
	env := buildHermesEnvironment(ctx, []string{"GOOD_NAME=explicit"})
	assert.Contains(t, env, "GOOD_NAME=explicit")
	assert.NotContains(t, env, "GOOD_NAME=ok")
}

func TestUserSkillCandidatesAndTextPayload(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	assert.Equal(t, []string{
		filepath.Join(home, ".agents", "skills", "target"),
		filepath.Join(home, ".codex", "skills", "target"),
	}, userSkillCandidates("target", filepath.Join(".agents", "skills"), filepath.Join(".codex", "skills")))
	assert.Nil(t, userSkillCandidates("", filepath.Join(".agents", "skills")))

	value, ok := (cliTextPayload{content: "hello"}).Text()
	assert.True(t, ok)
	assert.Equal(t, "hello", value)
}

func TestHermesGlobalSkillCandidatesFindsNestedSkill(t *testing.T) {
	hermesHome := t.TempDir()
	t.Setenv("HERMES_HOME", hermesHome)
	t.Setenv("HOME", t.TempDir())
	nested := filepath.Join(hermesHome, "skills", "bundled", "target")
	require.NoError(t, os.MkdirAll(nested, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(nested, "SKILL.md"), []byte("---\nname: target\ndescription: test\n---\n"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(hermesHome, "skills", "not-a-skill", "target"), 0o755))

	assert.Equal(t, []string{nested}, hermesGlobalSkillCandidates("target"))

	linkedSource := filepath.Join(t.TempDir(), "linked-target")
	require.NoError(t, os.MkdirAll(linkedSource, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(linkedSource, "SKILL.md"), []byte("---\nname: linked-target\ndescription: test\n---\n"), 0o600))
	linked := filepath.Join(hermesHome, "skills", "linked-target")
	require.NoError(t, os.Symlink(linkedSource, linked))
	assert.Equal(t, []string{linked}, hermesGlobalSkillCandidates("linked-target"))
	assert.Nil(t, hermesGlobalSkillCandidates(""))
}

func TestParseGenericJSONLProtocol(t *testing.T) {
	input := strings.Join([]string{
		`{"type":"assistant_message","text":"draft","session_id":"run-1"}`,
		`{"type":"future_event","ignored":true}`,
		`{"type":"usage","usage":{"turns":2,"input_tokens":10,"output_tokens":4,"cache_read_tokens":3,"cache_write_tokens":1}}`,
		`{"type":"result","text":"final","session_id":"run-1"}`,
		`{"type":"error","error":"reported failure"}`,
	}, "\n")
	result, err := parseGenericJSONL(strings.NewReader(input), "generic-model")
	require.NoError(t, err)
	assert.Equal(t, "final", result.FinalOutput)
	assert.Equal(t, "run-1", result.SessionID)
	assert.Equal(t, "reported failure", result.ErrorMsg)
	require.NotNil(t, result.Usage)
	assert.Equal(t, 2, result.Usage.Turns)
	assert.Equal(t, 3, result.Usage.CacheReadTokens)
	assert.Len(t, result.Events, 2)
}

func TestParseGenericJSONLRejectsMalformedJSONAndUsage(t *testing.T) {
	_, err := parseGenericJSONL(strings.NewReader("not-json\n"), "model")
	require.ErrorContains(t, err, "generic JSONL")
	_, err = parseGenericJSONL(strings.NewReader(`{"type":"usage","usage":"bad"}`), "model")
	require.ErrorContains(t, err, "usage")
}

func TestMergeCLIParseResultAndConstructors(t *testing.T) {
	dst := &cliParseResult{ModelID: "old"}
	src := &cliParseResult{
		ModelID: "new", SessionID: "session", ErrorMsg: "error",
		Usage: &models.UsageStats{InputTokens: 5},
	}
	mergeCLIParseResult(dst, src)
	assert.Equal(t, "new", dst.ModelID)
	assert.Equal(t, "session", dst.SessionID)
	assert.Equal(t, "error", dst.ErrorMsg)
	require.NotNil(t, dst.Usage)
	mergeCLIParseResult(nil, src)
	mergeCLIParseResult(dst, nil)

	codexEngine, ok := NewCodexCLIEngine("m", models.ExecutorConfig{}).(*cliEngine)
	require.True(t, ok)
	claudeEngine, ok := NewClaudeCLIEngine("m", models.ExecutorConfig{}).(*cliEngine)
	require.True(t, ok)
	hermesEngine, ok := NewHermesCLIEngine("m", models.ExecutorConfig{}).(*cliEngine)
	require.True(t, ok)
	assert.Equal(t, "codex-cli", codexEngine.name)
	assert.Equal(t, "claude-cli", claudeEngine.name)
	assert.Equal(t, "hermes-cli", hermesEngine.name)
}

func TestCLIEngineKeepWorkspaceAndExternalWorkspaceValidation(t *testing.T) {
	engine, ok := NewGenericCLIEngine("test-model", helperExecutorConfig(t, "text")).(*cliEngine)
	require.True(t, ok)
	require.NoError(t, engine.Initialize(context.Background()))
	engine.SetKeepWorkspace(true)
	resp, err := engine.Execute(context.Background(), &ExecutionRequest{Message: "hello"})
	require.NoError(t, err)
	require.NoError(t, engine.Shutdown(context.Background()))
	assert.DirExists(t, resp.WorkspaceDir)
	require.NoError(t, os.RemoveAll(resp.WorkspaceDir))

	other, ok := NewGenericCLIEngine("test-model", helperExecutorConfig(t, "text")).(*cliEngine)
	require.True(t, ok)
	require.NoError(t, other.Initialize(context.Background()))
	t.Cleanup(func() { require.NoError(t, other.Shutdown(context.Background())) })
	_, err = other.Execute(context.Background(), &ExecutionRequest{Message: "hello", WorkspaceDir: filepath.Join(t.TempDir(), "missing")})
	require.ErrorContains(t, err, "existing directory")
}
