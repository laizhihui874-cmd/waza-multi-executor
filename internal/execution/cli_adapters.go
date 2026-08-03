package execution

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/microsoft/waza/internal/agentevent"
	"github.com/microsoft/waza/internal/models"
)

func codexCLIAdapter() cliAdapter {
	return cliAdapter{
		DefaultCommand:  "codex",
		PromptTransport: "stdin",
		OutputFormat:    "codex-jsonl",
		SkillRoot:       filepath.Join(".agents", "skills"),
		InheritEnv:      true,
		BuildArgs: func(ctx cliBuildContext) ([]string, error) {
			args := []string{"exec", "--json", "--ephemeral", "--ignore-user-config", "--skip-git-repo-check", "--sandbox", "workspace-write", "--cd", ctx.WorkDir}
			if ctx.ModelID != "" {
				args = append(args, "--model", ctx.ModelID)
			}
			args = append(args, ctx.Config.Args...)
			return append(args, "-"), nil
		},
		KnownGlobalSkills: func(name string) []string {
			return userSkillCandidates(name, filepath.Join(".agents", "skills"), filepath.Join(".codex", "skills"))
		},
	}
}

func claudeCLIAdapter() cliAdapter {
	return cliAdapter{
		DefaultCommand:  "claude",
		PromptTransport: "stdin",
		OutputFormat:    "claude-jsonl",
		SkillRoot:       filepath.Join(".claude", "skills"),
		InheritEnv:      true,
		BuildArgs: func(ctx cliBuildContext) ([]string, error) {
			args := []string{
				"--print", "--output-format", "stream-json", "--no-session-persistence",
				"--verbose", "--permission-mode", "acceptEdits", "--setting-sources", "project",
			}
			if ctx.ModelID != "" {
				args = append(args, "--model", ctx.ModelID)
			}
			return append(args, ctx.Config.Args...), nil
		},
		KnownGlobalSkills: func(name string) []string {
			return userSkillCandidates(name, filepath.Join(".claude", "skills"))
		},
	}
}

func hermesCLIAdapter() cliAdapter {
	return cliAdapter{
		DefaultCommand:  "hermes",
		PromptTransport: "argument",
		OutputFormat:    "text",
		SkillRoot:       filepath.Join(".waza", "hermes-skills"),
		InheritEnv:      true,
		PrepareRuntime:  prepareHermesRuntime,
		BuildEnvironment: func(ctx cliBuildContext, env []string) []string {
			return buildHermesEnvironment(ctx, env)
		},
		BuildArgs: func(ctx cliBuildContext) ([]string, error) {
			args := append([]string(nil), ctx.Config.Args...)
			if ctx.ModelID != "" {
				args = append(args, "--model", ctx.ModelID)
			}
			if ctx.Config.SafeMode != nil && *ctx.Config.SafeMode {
				if ctx.SkillDir != "" {
					return nil, fmt.Errorf("hermes-cli executor_config.safe_mode disables skills and cannot be used for a skill evaluation")
				}
				args = append(args, "--safe-mode")
			}
			if ctx.SkillDir != "" {
				args = append(args, "--skills", filepath.Base(ctx.SkillDir))
			}
			args = append(args, "--usage-file", ctx.UsageFile, "--oneshot", ctx.Prompt)
			return args, nil
		},
		ParseUsageFile: parseHermesUsageFile,
		KnownGlobalSkills: func(name string) []string {
			return hermesGlobalSkillCandidates(name)
		},
	}
}

func prepareHermesRuntime(ctx cliBuildContext) error {
	home := filepath.Join(ctx.Workspace, ".waza", "hermes-home")
	externalSkills := filepath.Join(ctx.Workspace, ".waza", "hermes-skills")
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(externalSkills, 0o700); err != nil {
		return err
	}
	config := map[string]any{
		"skills": map[string]any{"external_dirs": []string{externalSkills}},
		"memory": map[string]any{"memory_enabled": false, "user_profile_enabled": false},
	}
	data, err := json.MarshalIndent(config, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(home, "config.yaml"), append(data, '\n'), 0o600)
}

func buildHermesEnvironment(ctx cliBuildContext, env []string) []string {
	sourceHome := strings.TrimSpace(os.Getenv("HERMES_HOME"))
	if sourceHome == "" {
		if userHome, err := os.UserHomeDir(); err == nil {
			sourceHome = filepath.Join(userHome, ".hermes")
		}
	}
	for _, entry := range readDotEnv(filepath.Join(sourceHome, ".env")) {
		name, _, ok := strings.Cut(entry, "=")
		if ok && !environmentContains(env, name) {
			env = append(env, entry)
		}
	}
	env = setEnv(env, "HERMES_HOME", filepath.Join(ctx.Workspace, ".waza", "hermes-home"))
	return setEnv(env, "HERMES_PLATFORM", "cli")
}

func readDotEnv(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var result []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
		name, value, ok := strings.Cut(line, "=")
		name = strings.TrimSpace(name)
		if !ok || !validEnvironmentName(name) {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && ((value[0] == '\'' && value[len(value)-1] == '\'') || (value[0] == '"' && value[len(value)-1] == '"')) {
			value = value[1 : len(value)-1]
		}
		result = append(result, name+"="+value)
	}
	return result
}

func validEnvironmentName(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || r == '_' || (i > 0 && r >= '0' && r <= '9') {
			continue
		}
		return false
	}
	return true
}

func environmentContains(env []string, name string) bool {
	prefix := name + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return true
		}
	}
	return false
}

func userSkillCandidates(name string, roots ...string) []string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" || name == "" {
		return nil
	}
	result := make([]string, 0, len(roots))
	for _, root := range roots {
		result = append(result, filepath.Join(home, root, name))
	}
	return result
}

func hermesGlobalSkillCandidates(name string) []string {
	if name == "" {
		return nil
	}
	var roots []string
	if configuredHome := strings.TrimSpace(os.Getenv("HERMES_HOME")); configuredHome != "" {
		roots = append(roots, filepath.Join(configuredHome, "skills"))
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		defaultRoot := filepath.Join(home, ".hermes", "skills")
		if len(roots) == 0 || roots[0] != defaultRoot {
			roots = append(roots, defaultRoot)
		}
	}

	var result []string
	seen := make(map[string]struct{})
	for _, root := range roots {
		_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				if path == root {
					return filepath.SkipDir
				}
				return nil
			}
			if path == root {
				return nil
			}
			if strings.EqualFold(entry.Name(), name) {
				info, statErr := os.Stat(path)
				if statErr == nil && info.IsDir() && loadSkillDefinition(path) != nil {
					if _, ok := seen[path]; !ok {
						seen[path] = struct{}{}
						result = append(result, path)
					}
				}
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return nil
			}
			return nil
		})
	}
	return result
}

func parseCodexJSONL(reader io.Reader, modelID string) (*cliParseResult, error) {
	result := &cliParseResult{ModelID: modelID}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), defaultStdoutLimit)
	sequence := 0
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var event struct {
			Type     string `json:"type"`
			ThreadID string `json:"thread_id"`
			Usage    struct {
				InputTokens       int `json:"input_tokens"`
				CachedInputTokens int `json:"cached_input_tokens"`
				OutputTokens      int `json:"output_tokens"`
			} `json:"usage"`
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
			Item struct {
				ID               string         `json:"id"`
				Type             string         `json:"type"`
				Text             string         `json:"text"`
				Command          string         `json:"command"`
				AggregatedOutput string         `json:"aggregated_output"`
				ExitCode         *int           `json:"exit_code"`
				Status           string         `json:"status"`
				Server           string         `json:"server"`
				Tool             string         `json:"tool"`
				Arguments        map[string]any `json:"arguments"`
				Result           any            `json:"result"`
				Error            any            `json:"error"`
			} `json:"item"`
		}
		if err := json.Unmarshal(line, &event); err != nil {
			return result, fmt.Errorf("codex JSONL line %d: %w", lineNumber, err)
		}
		switch event.Type {
		case "thread.started":
			result.SessionID = event.ThreadID
		case "item.completed":
			switch event.Item.Type {
			case "agent_message":
				if event.Item.Text != "" {
					result.FinalOutput = event.Item.Text
					result.Events = append(result.Events, agentevent.New(agentevent.KindAssistantMessage, cliTextPayload{content: event.Item.Text}))
				}
			case "command_execution":
				sequence++
				success := event.Item.ExitCode == nil || *event.Item.ExitCode == 0
				toolEvent := models.ToolEvent{
					Sequence: sequence, ToolCallID: event.Item.ID, ToolName: "command_execution",
					Args: map[string]any{"command": event.Item.Command}, Result: event.Item.AggregatedOutput, Success: success,
				}
				if !success {
					toolEvent.Error = fmt.Sprintf("exit code %d", *event.Item.ExitCode)
				}
				result.ToolEvents = append(result.ToolEvents, toolEvent)
				result.ToolCalls = append(result.ToolCalls, models.ToolCall{
					ID: event.Item.ID, Name: "command_execution",
					Arguments: models.ToolCallArgs{Command: event.Item.Command}, Success: success,
				})
			case "mcp_tool_call":
				sequence++
				name := event.Item.Tool
				if event.Item.Server != "" {
					name = event.Item.Server + "/" + name
				}
				success := event.Item.Error == nil
				result.ToolEvents = append(result.ToolEvents, models.ToolEvent{
					Sequence: sequence, ToolCallID: event.Item.ID, ToolName: name,
					Args: event.Item.Arguments, Result: event.Item.Result, Success: success,
				})
				result.ToolCalls = append(result.ToolCalls, models.ToolCall{
					ID: event.Item.ID, Name: name,
					Arguments: models.ToolCallArgs{Extra: event.Item.Arguments}, Success: success,
				})
			}
		case "turn.completed":
			result.Usage = &models.UsageStats{
				Turns: 1, InputTokens: event.Usage.InputTokens,
				CacheReadTokens: event.Usage.CachedInputTokens, OutputTokens: event.Usage.OutputTokens,
			}
		case "turn.failed", "error":
			if event.Error.Message != "" {
				result.ErrorMsg = event.Error.Message
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return result, fmt.Errorf("read codex JSONL: %w", err)
	}
	return result, nil
}

func parseClaudeJSONL(reader io.Reader, modelID string) (*cliParseResult, error) {
	result := &cliParseResult{ModelID: modelID}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), defaultStdoutLimit)
	for lineNumber := 1; scanner.Scan(); lineNumber++ {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var event struct {
			Type      string `json:"type"`
			Subtype   string `json:"subtype"`
			Result    string `json:"result"`
			SessionID string `json:"session_id"`
			IsError   bool   `json:"is_error"`
			Error     string `json:"error"`
			Usage     struct {
				InputTokens              int `json:"input_tokens"`
				OutputTokens             int `json:"output_tokens"`
				CacheReadInputTokens     int `json:"cache_read_input_tokens"`
				CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
			} `json:"usage"`
			Message struct {
				Model string `json:"model"`
				Usage struct {
					InputTokens              int `json:"input_tokens"`
					OutputTokens             int `json:"output_tokens"`
					CacheReadInputTokens     int `json:"cache_read_input_tokens"`
					CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
				} `json:"usage"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(line, &event); err != nil {
			return result, fmt.Errorf("claude JSONL line %d: %w", lineNumber, err)
		}
		if event.SessionID != "" {
			result.SessionID = event.SessionID
		}
		switch event.Type {
		case "assistant":
			for _, block := range event.Message.Content {
				if block.Type == "text" && block.Text != "" {
					result.FinalOutput = block.Text
					result.Events = append(result.Events, agentevent.New(agentevent.KindAssistantMessage, cliTextPayload{content: block.Text}))
				}
			}
			if event.Message.Model != "" {
				result.ModelID = event.Message.Model
			}
			if event.Message.Usage.InputTokens != 0 || event.Message.Usage.OutputTokens != 0 {
				result.Usage = &models.UsageStats{
					Turns: 1, InputTokens: event.Message.Usage.InputTokens,
					OutputTokens:     event.Message.Usage.OutputTokens,
					CacheReadTokens:  event.Message.Usage.CacheReadInputTokens,
					CacheWriteTokens: event.Message.Usage.CacheCreationInputTokens,
				}
			}
		case "result":
			if event.Result != "" {
				result.FinalOutput = event.Result
			}
			if event.Usage.InputTokens != 0 || event.Usage.OutputTokens != 0 {
				result.Usage = &models.UsageStats{
					Turns: 1, InputTokens: event.Usage.InputTokens, OutputTokens: event.Usage.OutputTokens,
					CacheReadTokens:  event.Usage.CacheReadInputTokens,
					CacheWriteTokens: event.Usage.CacheCreationInputTokens,
				}
			}
			if event.IsError {
				result.ErrorMsg = event.Error
				if result.ErrorMsg == "" {
					result.ErrorMsg = event.Result
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return result, fmt.Errorf("read claude JSONL: %w", err)
	}
	return result, nil
}

func parseHermesUsageFile(path, defaultModelID string) (*cliParseResult, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &cliParseResult{ModelID: defaultModelID}, nil
		}
		return nil, fmt.Errorf("read Hermes usage file: %w", err)
	}
	var report struct {
		InputTokens      int    `json:"input_tokens"`
		OutputTokens     int    `json:"output_tokens"`
		CacheReadTokens  int    `json:"cache_read_tokens"`
		CacheWriteTokens int    `json:"cache_write_tokens"`
		APICalls         int    `json:"api_calls"`
		Model            string `json:"model"`
		Provider         string `json:"provider"`
		SessionID        string `json:"session_id"`
		Failed           bool   `json:"failed"`
		Failure          string `json:"failure"`
	}
	if err := json.Unmarshal(data, &report); err != nil {
		return nil, fmt.Errorf("parse Hermes usage file: %w", err)
	}
	result := &cliParseResult{
		ModelID: report.Model, SessionID: report.SessionID,
		Usage: &models.UsageStats{
			Turns: report.APICalls, InputTokens: report.InputTokens, OutputTokens: report.OutputTokens,
			CacheReadTokens: report.CacheReadTokens, CacheWriteTokens: report.CacheWriteTokens,
			Provider: report.Provider,
		},
	}
	if result.ModelID == "" {
		result.ModelID = defaultModelID
	}
	if report.Failed {
		result.ErrorMsg = report.Failure
		if result.ErrorMsg == "" {
			result.ErrorMsg = "Hermes reported a failed run"
		}
	}
	return result, nil
}
