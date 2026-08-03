package execution

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/microsoft/waza/internal/agentevent"
	"github.com/microsoft/waza/internal/models"
)

const (
	defaultStdoutLimit = 16 << 20
	defaultStderrLimit = 1 << 20
)

type cliAdapter struct {
	DefaultCommand    string
	PromptTransport   string
	OutputFormat      string
	SkillRoot         string
	InheritEnv        bool
	PrepareRuntime    func(cliBuildContext) error
	BuildEnvironment  func(cliBuildContext, []string) []string
	BuildArgs         func(cliBuildContext) ([]string, error)
	Parse             func(io.Reader, string) (*cliParseResult, error)
	ParseUsageFile    func(string, string) (*cliParseResult, error)
	KnownGlobalSkills func(string) []string
}

type cliBuildContext struct {
	Config    models.ExecutorConfig
	ModelID   string
	Prompt    string
	Workspace string
	WorkDir   string
	SkillDir  string
	UsageFile string
}

type cliParseResult struct {
	FinalOutput string
	Events      []agentevent.Event
	ToolCalls   []models.ToolCall
	ToolEvents  []models.ToolEvent
	Usage       *models.UsageStats
	SessionID   string
	ModelID     string
	ErrorMsg    string
}

type cliTextPayload struct{ content string }

func (p cliTextPayload) Text() (string, bool) { return p.content, p.content != "" }

type cliEngine struct {
	name           string
	defaultModelID string
	config         models.ExecutorConfig
	adapter        cliAdapter
	commandPath    string
	initialized    atomic.Bool
	keepWorkspace  atomic.Bool
	shutdownOnce   sync.Once
	shutdownErr    error

	mu           sync.Mutex
	workspaces   []string
	gitResources []GitResource
	usage        map[string]*models.UsageStats
}

func newCLIEngine(name, modelID string, cfg models.ExecutorConfig, adapter cliAdapter) *cliEngine {
	return &cliEngine{
		name:           name,
		defaultModelID: modelID,
		config:         cfg,
		adapter:        adapter,
		usage:          make(map[string]*models.UsageStats),
	}
}

func NewGenericCLIEngine(modelID string, cfg models.ExecutorConfig) AgentEngine {
	return newCLIEngine("generic-cli", modelID, cfg, cliAdapter{
		PromptTransport: "stdin",
		OutputFormat:    "text",
		SkillRoot:       filepath.Join(".waza", "skills"),
	})
}

func NewCodexCLIEngine(modelID string, cfg models.ExecutorConfig) AgentEngine {
	return newCLIEngine("codex-cli", modelID, cfg, codexCLIAdapter())
}

func NewClaudeCLIEngine(modelID string, cfg models.ExecutorConfig) AgentEngine {
	return newCLIEngine("claude-cli", modelID, cfg, claudeCLIAdapter())
}

func NewHermesCLIEngine(modelID string, cfg models.ExecutorConfig) AgentEngine {
	return newCLIEngine("hermes-cli", modelID, cfg, hermesCLIAdapter())
}

func (e *cliEngine) SetKeepWorkspace(keep bool) { e.keepWorkspace.Store(keep) }

func (e *cliEngine) Initialize(context.Context) error {
	if e.initialized.Load() {
		return nil
	}
	command := strings.TrimSpace(e.config.Command)
	if command == "" {
		command = e.adapter.DefaultCommand
	}
	if command == "" {
		return fmt.Errorf("executor %q requires executor_config.command", e.name)
	}
	path, err := exec.LookPath(command)
	if err != nil {
		return fmt.Errorf("executor %q executable %q was not found: %w", e.name, command, err)
	}
	e.commandPath = path
	if err := e.validateConfig(); err != nil {
		return err
	}
	e.initialized.Store(true)
	return nil
}

func (e *cliEngine) validateConfig() error {
	promptTransport := e.promptTransport()
	if promptTransport != "stdin" && promptTransport != "argument" {
		return fmt.Errorf("executor %q prompt_transport must be stdin or argument, got %q", e.name, promptTransport)
	}
	format := e.outputFormat()
	switch format {
	case "text", "jsonl", "codex-jsonl", "claude-jsonl":
		return nil
	default:
		return fmt.Errorf("executor %q output_format %q is not supported", e.name, format)
	}
}

func (e *cliEngine) promptTransport() string {
	if value := strings.TrimSpace(e.config.PromptTransport); value != "" {
		return value
	}
	if e.adapter.PromptTransport != "" {
		return e.adapter.PromptTransport
	}
	return "stdin"
}

func (e *cliEngine) outputFormat() string {
	if value := strings.TrimSpace(e.config.OutputFormat); value != "" {
		return value
	}
	if e.adapter.OutputFormat != "" {
		return e.adapter.OutputFormat
	}
	return "text"
}

func (e *cliEngine) Execute(ctx context.Context, req *ExecutionRequest) (*ExecutionResponse, error) {
	if !e.initialized.Load() {
		return nil, fmt.Errorf("executor %q was not initialized", e.name)
	}
	if req == nil {
		return nil, fmt.Errorf("executor %q received a nil execution request", e.name)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req.SessionID != "" {
		return nil, fmt.Errorf("executor %q does not support resuming sessions", e.name)
	}

	start := time.Now()
	workspaceDir, err := e.prepareWorkspace(ctx, req)
	if err != nil {
		return nil, err
	}
	workDir, err := ResolveWorkDir(workspaceDir, req.WorkDir)
	if err != nil {
		return nil, err
	}
	skillDir, err := e.materializeNativeSkill(req, workspaceDir)
	if err != nil {
		return nil, err
	}
	if err := e.materializeInstructions(req.Instructions, workspaceDir); err != nil {
		return nil, err
	}

	modelID := e.defaultModelID
	if req.ModelID != "" {
		modelID = req.ModelID
	}
	usageFile := filepath.Join(workspaceDir, ".waza", "usage.json")
	buildCtx := cliBuildContext{
		Config: e.config, ModelID: modelID, Prompt: req.Message,
		Workspace: workspaceDir, WorkDir: workDir, SkillDir: skillDir, UsageFile: usageFile,
	}
	if e.adapter.PrepareRuntime != nil {
		if err := e.adapter.PrepareRuntime(buildCtx); err != nil {
			return nil, fmt.Errorf("prepare executor %q runtime: %w", e.name, err)
		}
	}
	args, err := e.buildArgs(buildCtx)
	if err != nil {
		return nil, err
	}

	cmd := exec.CommandContext(ctx, e.commandPath, args...)
	configureProcessGroup(cmd)
	cmd.Cancel = func() error {
		terminateProcessGroup(cmd)
		return nil
	}
	cmd.WaitDelay = 2 * time.Second
	cmd.Dir = workDir
	cmd.Env = e.buildEnvironment(buildCtx)
	if e.promptTransport() == "stdin" {
		cmd.Stdin = strings.NewReader(req.Message)
	}
	stdout := &limitedBuffer{limit: defaultStdoutLimit}
	stderr := &limitedBuffer{limit: defaultStderrLimit}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	runErr := cmd.Run()
	if runErr != nil {
		terminateProcessGroup(cmd)
	}
	if ctx.Err() != nil {
		return &ExecutionResponse{
			ModelID: modelID, DurationMs: time.Since(start).Milliseconds(), Success: false,
			ErrorMsg: ctx.Err().Error(), WorkspaceDir: workspaceDir,
		}, ctx.Err()
	}

	parsed, parseErr := e.parseOutput(bytes.NewReader(stdout.Bytes()), usageFile, modelID)
	if parsed == nil {
		parsed = &cliParseResult{ModelID: modelID}
	}
	if parsed.ModelID == "" {
		parsed.ModelID = modelID
	}
	resp := &ExecutionResponse{
		FinalOutput: parsed.FinalOutput, Events: parsed.Events, ModelID: parsed.ModelID,
		DurationMs: time.Since(start).Milliseconds(), ToolCalls: parsed.ToolCalls,
		ToolEvents: parsed.ToolEvents, ErrorMsg: parsed.ErrorMsg, Success: runErr == nil && parseErr == nil,
		SessionID: parsed.SessionID, Usage: parsed.Usage, WorkspaceDir: workspaceDir,
	}
	if !req.SkipWorkspaceCapture {
		resp.WorkspaceFiles = captureCLIWorkspaceFiles(workspaceDir, e.adapter.SkillRoot)
	}
	if resp.SessionID == "" {
		resp.SessionID = fmt.Sprintf("%s-%d", e.name, start.UnixNano())
	}
	if resp.Usage != nil {
		e.mu.Lock()
		e.usage[resp.SessionID] = cloneUsage(resp.Usage)
		e.mu.Unlock()
	}
	if runErr != nil {
		detail := strings.TrimSpace(parsed.ErrorMsg)
		stderrDetail := strings.TrimSpace(stderr.String())
		if detail != "" && stderrDetail != "" {
			detail += "\n" + stderrDetail
		} else if detail == "" {
			detail = stderrDetail
		}
		if strings.TrimSpace(detail) == "" && parsed.FinalOutput != "" {
			detail = parsed.FinalOutput
		}
		err := e.processError(runErr, detail)
		resp.ErrorMsg = err.Error()
		return resp, err
	}
	if parseErr != nil {
		resp.ErrorMsg = parseErr.Error()
		return resp, parseErr
	}
	if parsed.ErrorMsg != "" {
		resp.Success = false
		return resp, errors.New(parsed.ErrorMsg)
	}
	return resp, nil
}

func (e *cliEngine) buildArgs(ctx cliBuildContext) ([]string, error) {
	if e.adapter.BuildArgs != nil {
		return e.adapter.BuildArgs(ctx)
	}
	args := append([]string(nil), e.config.Args...)
	values := map[string]string{
		"{workspace}": ctx.Workspace, "{workdir}": ctx.WorkDir,
		"{skill_dir}": ctx.SkillDir, "{model}": ctx.ModelID,
	}
	if e.promptTransport() == "argument" {
		values["{prompt}"] = ctx.Prompt
	}
	promptPlaceholderUsed := false
	for i, arg := range args {
		if strings.Contains(arg, "{prompt}") {
			if e.promptTransport() != "argument" {
				return nil, fmt.Errorf("executor %q uses {prompt} in args but prompt_transport is not argument", e.name)
			}
			promptPlaceholderUsed = true
		}
		for placeholder, value := range values {
			arg = strings.ReplaceAll(arg, placeholder, value)
		}
		args[i] = arg
	}
	if e.promptTransport() == "argument" && !promptPlaceholderUsed {
		args = append(args, ctx.Prompt)
	}
	return args, nil
}

func (e *cliEngine) buildEnvironment(ctx cliBuildContext) []string {
	var env []string
	if e.adapter.InheritEnv {
		env = append(env, os.Environ()...)
	} else {
		for _, name := range []string{"PATH", "TMPDIR"} {
			if value, ok := os.LookupEnv(name); ok {
				env = append(env, name+"="+value)
			}
		}
		for _, name := range e.config.EnvAllowlist {
			if value, ok := os.LookupEnv(name); ok {
				env = append(env, name+"="+value)
			}
		}
	}
	env = setEnv(env, "WAZA_EXECUTOR", e.name)
	env = setEnv(env, "WAZA_WORKSPACE", ctx.Workspace)
	env = setEnv(env, "WAZA_WORKDIR", ctx.WorkDir)
	env = setEnv(env, "WAZA_SKILL_DIR", ctx.SkillDir)
	env = setEnv(env, "WAZA_MODEL", ctx.ModelID)
	if e.adapter.BuildEnvironment != nil {
		env = e.adapter.BuildEnvironment(ctx, env)
	}
	return env
}

func setEnv(env []string, name, value string) []string {
	prefix := name + "="
	for i, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}

func (e *cliEngine) parseOutput(stdout io.Reader, usageFile, modelID string) (*cliParseResult, error) {
	var result *cliParseResult
	var err error
	if e.adapter.Parse != nil {
		result, err = e.adapter.Parse(stdout, modelID)
	} else {
		switch e.outputFormat() {
		case "text":
			data, readErr := io.ReadAll(stdout)
			if readErr != nil {
				return nil, readErr
			}
			text := strings.TrimSpace(string(data))
			result = &cliParseResult{FinalOutput: text, ModelID: modelID}
			if text != "" {
				result.Events = []agentevent.Event{agentevent.New(agentevent.KindAssistantMessage, cliTextPayload{content: text})}
			}
		case "jsonl":
			result, err = parseGenericJSONL(stdout, modelID)
		case "codex-jsonl":
			result, err = parseCodexJSONL(stdout, modelID)
		case "claude-jsonl":
			result, err = parseClaudeJSONL(stdout, modelID)
		}
	}
	if err != nil {
		return result, err
	}
	if e.adapter.ParseUsageFile != nil {
		usageResult, usageErr := e.adapter.ParseUsageFile(usageFile, modelID)
		if usageErr != nil {
			return result, usageErr
		}
		mergeCLIParseResult(result, usageResult)
	}
	return result, nil
}

func mergeCLIParseResult(dst, src *cliParseResult) {
	if dst == nil || src == nil {
		return
	}
	if src.Usage != nil {
		dst.Usage = src.Usage
	}
	if src.SessionID != "" {
		dst.SessionID = src.SessionID
	}
	if src.ModelID != "" {
		dst.ModelID = src.ModelID
	}
	if src.ErrorMsg != "" {
		dst.ErrorMsg = src.ErrorMsg
	}
}

func (e *cliEngine) processError(runErr error, stderr string) error {
	message := strings.TrimSpace(stderr)
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		if message == "" {
			return fmt.Errorf("executor %q exited with exit code %d", e.name, exitErr.ExitCode())
		}
		return fmt.Errorf("executor %q exited with exit code %d: %s", e.name, exitErr.ExitCode(), message)
	}
	if message != "" {
		return fmt.Errorf("executor %q failed: %w: %s", e.name, runErr, message)
	}
	return fmt.Errorf("executor %q failed: %w", e.name, runErr)
}

func (e *cliEngine) prepareWorkspace(ctx context.Context, req *ExecutionRequest) (string, error) {
	if req.WorkspaceDir != "" {
		if info, err := os.Stat(req.WorkspaceDir); err != nil || !info.IsDir() {
			return "", fmt.Errorf("executor %q workspace %q is not an existing directory", e.name, req.WorkspaceDir)
		}
		return req.WorkspaceDir, nil
	}
	workspaceDir, err := os.MkdirTemp("", "waza-"+e.name+"-*")
	if err != nil {
		return "", fmt.Errorf("create executor %q workspace: %w", e.name, err)
	}
	if err := setupWorkspaceResources(workspaceDir, req.Resources); err != nil {
		_ = os.RemoveAll(workspaceDir)
		return "", fmt.Errorf("setup executor %q workspace resources: %w", e.name, err)
	}
	gitResources, err := CloneGitResources(ctx, req.GitResources, workspaceDir)
	if err != nil {
		_ = os.RemoveAll(workspaceDir)
		return "", fmt.Errorf("setup executor %q git resources: %w", e.name, err)
	}
	e.mu.Lock()
	e.workspaces = append(e.workspaces, workspaceDir)
	e.gitResources = append(e.gitResources, gitResources...)
	e.mu.Unlock()
	return workspaceDir, nil
}

func (e *cliEngine) materializeNativeSkill(req *ExecutionRequest, workspaceDir string) (string, error) {
	if req.NoSkills || len(req.SkillPaths) == 0 {
		return "", nil
	}
	source, skillName := findTargetSkill(req.SkillPaths, req.SkillName)
	if source == "" {
		return "", fmt.Errorf("executor %q could not find target skill %q in configured skill_directories", e.name, req.SkillName)
	}
	if !e.config.AllowSkillShadowing && e.adapter.KnownGlobalSkills != nil {
		for _, candidate := range e.adapter.KnownGlobalSkills(skillName) {
			if info, err := os.Stat(candidate); err == nil && info.IsDir() {
				return "", fmt.Errorf("executor %q found globally visible skill %q at %s; baseline would be contaminated (set executor_config.allow_skill_shadowing only if intentional)", e.name, skillName, candidate)
			}
		}
	}
	root := e.adapter.SkillRoot
	if root == "" {
		root = filepath.Join(".waza", "skills")
	}
	destination := filepath.Join(workspaceDir, root, filepath.Base(source))
	if err := copySkillDirectory(source, destination); err != nil {
		return "", fmt.Errorf("materialize native skill %q for executor %q: %w", skillName, e.name, err)
	}
	return destination, nil
}

func findTargetSkill(paths []string, targetName string) (string, string) {
	for _, path := range paths {
		if skill := loadSkillDefinition(path); skill != nil && (targetName == "" || strings.EqualFold(skill.Name, targetName)) {
			return skill.Dir, skill.Name
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
				continue
			}
			if skill := loadSkillDefinition(filepath.Join(path, entry.Name())); skill != nil &&
				(targetName == "" || strings.EqualFold(skill.Name, targetName)) {
				return skill.Dir, skill.Name
			}
		}
	}
	return "", ""
}

func copySkillDirectory(source, destination string) error {
	if _, err := os.Stat(destination); err == nil {
		return fmt.Errorf("destination %q already exists", destination)
	} else if !os.IsNotExist(err) {
		return err
	}
	return filepath.WalkDir(source, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("skill contains unsupported symbolic link %q", path)
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		target := filepath.Join(destination, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}

func (e *cliEngine) materializeInstructions(instructions []InstructionFile, workspaceDir string) error {
	if len(instructions) == 0 {
		return nil
	}
	var content strings.Builder
	content.WriteString("# Waza evaluation instructions\n\n")
	for _, instruction := range instructions {
		fmt.Fprintf(&content, "## %s\n\n%s\n\n", instruction.Path, instruction.Content)
	}
	for _, filename := range instructionFilenames(e.name) {
		path := filepath.Join(workspaceDir, filename)
		if _, err := os.Stat(path); err == nil {
			continue
		}
		if err := os.WriteFile(path, []byte(content.String()), 0o644); err != nil {
			return fmt.Errorf("materialize %s instructions: %w", e.name, err)
		}
	}
	return nil
}

func instructionFilenames(executor string) []string {
	switch executor {
	case "claude-cli":
		return []string{"CLAUDE.md"}
	case "codex-cli", "hermes-cli":
		return []string{"AGENTS.md"}
	default:
		return nil
	}
}

func captureCLIWorkspaceFiles(dir, skillRoot string) map[string][]byte {
	files := captureWorkspaceFiles(dir)
	for path := range files {
		if strings.HasPrefix(path, ".waza/") || (skillRoot != "" && (path == filepath.ToSlash(skillRoot) || strings.HasPrefix(path, filepath.ToSlash(skillRoot)+"/"))) {
			delete(files, path)
		}
	}
	return files
}

func (e *cliEngine) Shutdown(context.Context) error {
	e.shutdownOnce.Do(func() {
		e.mu.Lock()
		gitResources := append([]GitResource(nil), e.gitResources...)
		workspaces := append([]string(nil), e.workspaces...)
		e.gitResources = nil
		e.workspaces = nil
		e.mu.Unlock()
		for _, resource := range gitResources {
			if err := resource.Cleanup(context.Background()); err != nil && e.shutdownErr == nil {
				e.shutdownErr = err
			}
		}
		for _, workspace := range workspaces {
			if e.keepWorkspace.Load() {
				fmt.Fprintf(os.Stderr, "Workspace preserved: %s\n", workspace)
				continue
			}
			if err := os.RemoveAll(workspace); err != nil && e.shutdownErr == nil {
				e.shutdownErr = err
			}
		}
	})
	return e.shutdownErr
}

func (e *cliEngine) SessionUsage(sessionID string) *models.UsageStats {
	e.mu.Lock()
	defer e.mu.Unlock()
	return cloneUsage(e.usage[sessionID])
}

func cloneUsage(usage *models.UsageStats) *models.UsageStats {
	if usage == nil {
		return nil
	}
	clone := *usage
	if usage.ModelMetrics != nil {
		clone.ModelMetrics = make(map[string]models.ModelUsage, len(usage.ModelMetrics))
		for key, value := range usage.ModelMetrics {
			clone.ModelMetrics[key] = value
		}
	}
	return &clone
}

type limitedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	originalLength := len(p)
	remaining := b.limit - b.buf.Len()
	if remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
			b.truncated = true
		}
		_, _ = b.buf.Write(p)
	} else if len(p) > 0 {
		b.truncated = true
	}
	return originalLength, nil
}

func (b *limitedBuffer) Bytes() []byte { return b.buf.Bytes() }

func (b *limitedBuffer) String() string {
	value := b.buf.String()
	if b.truncated {
		value += "\n[output truncated by waza]"
	}
	return value
}

func parseGenericJSONL(reader io.Reader, modelID string) (*cliParseResult, error) {
	result := &cliParseResult{ModelID: modelID}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), defaultStdoutLimit)
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var event struct {
			Type      string          `json:"type"`
			Text      string          `json:"text"`
			SessionID string          `json:"session_id"`
			Error     string          `json:"error"`
			Usage     json.RawMessage `json:"usage"`
		}
		if err := json.Unmarshal(line, &event); err != nil {
			return result, fmt.Errorf("generic JSONL line %d: %w", lineNumber, err)
		}
		switch event.Type {
		case "assistant_message", "result":
			if event.Text != "" {
				result.FinalOutput = event.Text
				result.Events = append(result.Events, agentevent.New(agentevent.KindAssistantMessage, cliTextPayload{content: event.Text}))
			}
		case "usage":
			usage, err := parseUsageJSON(event.Usage)
			if err != nil {
				return result, fmt.Errorf("generic JSONL line %d usage: %w", lineNumber, err)
			}
			result.Usage = usage
		case "error":
			result.ErrorMsg = event.Error
		}
		if event.SessionID != "" {
			result.SessionID = event.SessionID
		}
	}
	if err := scanner.Err(); err != nil {
		return result, err
	}
	return result, nil
}

func parseUsageJSON(data []byte) (*models.UsageStats, error) {
	if len(data) == 0 || bytes.Equal(data, []byte("null")) {
		return nil, nil
	}
	var raw struct {
		Turns            int `json:"turns"`
		InputTokens      int `json:"input_tokens"`
		OutputTokens     int `json:"output_tokens"`
		CacheReadTokens  int `json:"cache_read_tokens"`
		CacheWriteTokens int `json:"cache_write_tokens"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	return &models.UsageStats{
		Turns: raw.Turns, InputTokens: raw.InputTokens, OutputTokens: raw.OutputTokens,
		CacheReadTokens: raw.CacheReadTokens, CacheWriteTokens: raw.CacheWriteTokens,
	}, nil
}
