package scaffold

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/microsoft/waza/internal/validation"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateName(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
		errMsg  string
	}{
		{"valid kebab-case", "my-skill", false, ""},
		{"valid simple", "skill", false, ""},
		{"empty", "", true, "must not be empty"},
		{"path traversal dots", "../evil", true, "invalid path characters"},
		{"forward slash", "a/b", true, "invalid path characters"},
		{"backslash", "a\\b", true, "invalid path characters"},
		{"traversal masked by clean", "a/..", true, "invalid path characters"},
		{"nested traversal", "a/../b", true, "invalid path characters"},
		{"dot only", ".", true, "invalid path characters"},
		{"double dot embedded", "foo..bar", true, "invalid path characters"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateName(tc.input)
			if tc.wantErr {
				assert.Error(t, err)
				if tc.errMsg != "" {
					assert.Contains(t, err.Error(), tc.errMsg)
				}
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestTitleCase(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"my-skill", "My Skill"},
		{"code-analyzer", "Code Analyzer"},
		{"skill", "Skill"},
		{"a-b-c", "A B C"},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			assert.Equal(t, tc.want, TitleCase(tc.input))
		})
	}
}

func TestEvalYAML(t *testing.T) {
	content := EvalYAML("code-analyzer", "copilot-sdk", "claude-sonnet-4.6")

	assert.Contains(t, content, "name: code-analyzer-eval")
	assert.Contains(t, content, "skill: code-analyzer")
	assert.Contains(t, content, `schemaVersion: "1.0"`)
	assert.Contains(t, content, "executor: copilot-sdk")
	assert.Contains(t, content, "model: claude-sonnet-4.6")
	assert.Contains(t, content, "type: code")
	assert.Contains(t, content, "type: text")
	assert.Contains(t, content, `"tasks/*.yaml"`)
}

func TestEvalYAML_CustomEngine(t *testing.T) {
	content := EvalYAML("my-skill", "codex-cli", "gpt-5.5")

	assert.Contains(t, content, "executor: codex-cli")
	assert.Contains(t, content, "model: gpt-5.5")
}

func TestEvalYAML_GenericCLIIncludesRequiredExecutorConfig(t *testing.T) {
	content := EvalYAML("my-skill", "generic-cli", "local-model")

	assert.Contains(t, content, "executor: generic-cli")
	assert.Contains(t, content, "command: my-agent")
	errList := validation.ValidateEvalBytes([]byte(content))
	require.Empty(t, errList, "generic-cli scaffold should pass schema validation: %v", errList)
}

func TestEvalYAMLWithTaskGlob(t *testing.T) {
	content := EvalYAMLWithTaskGlob("my-skill", "claude-cli", "claude-sonnet-4.6", "tasks/*.waza-task.yaml")

	assert.Contains(t, content, `"tasks/*.waza-task.yaml"`)
}

func TestTaskFiles(t *testing.T) {
	tasks := TaskFiles("my-skill")

	assert.Contains(t, tasks, "basic-usage.yaml")
	assert.Contains(t, tasks, "edge-case.yaml")
	assert.Contains(t, tasks, "should-not-trigger.yaml")
	assert.Len(t, tasks, 3)

	assert.Contains(t, tasks["basic-usage.yaml"], "id: basic-usage-001")
	assert.Contains(t, tasks["basic-usage.yaml"], `prompt: "Read sample.py and explain the function it contains."`)
	assert.Contains(t, tasks["basic-usage.yaml"], "path: sample.py")
	assert.Contains(t, tasks["edge-case.yaml"], "id: edge-case-001")
	assert.Contains(t, tasks["should-not-trigger.yaml"], "id: should-not-trigger-001")
}

func TestTaskFilesWithSuffix(t *testing.T) {
	tasks := TaskFilesWithSuffix(".waza-task.yaml")

	assert.Contains(t, tasks, "basic-usage.waza-task.yaml")
	assert.Contains(t, tasks, "edge-case.waza-task.yaml")
	assert.Contains(t, tasks, "should-not-trigger.waza-task.yaml")
	assert.Len(t, tasks, 3)
}

func TestFixture(t *testing.T) {
	content := Fixture()
	assert.Contains(t, content, "def hello(name):")
}

func TestReadProjectDefaults(t *testing.T) {
	dir := t.TempDir()
	origDir, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() { os.Chdir(origDir) }) //nolint:errcheck // best-effort cleanup

	// No .waza.yaml → defaults
	engine, model := ReadProjectDefaults()
	assert.Equal(t, "copilot-sdk", engine)
	assert.Equal(t, "claude-sonnet-4.6", model)
}

func TestReadProjectDefaults_WithConfig(t *testing.T) {
	dir := t.TempDir()
	origDir, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() { os.Chdir(origDir) }) //nolint:errcheck // best-effort cleanup

	wazaConfig := "defaults:\n  engine: codex-cli\n  model: gpt-5.5\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".waza.yaml"), []byte(wazaConfig), 0o644))

	engine, model := ReadProjectDefaults()
	assert.Equal(t, "codex-cli", engine)
	assert.Equal(t, "gpt-5.5", model)
}

func TestReadProjectFiles_WithConfig(t *testing.T) {
	dir := t.TempDir()
	origDir, err := os.Getwd()
	require.NoError(t, err)
	require.NoError(t, os.Chdir(dir))
	t.Cleanup(func() { os.Chdir(origDir) }) //nolint:errcheck // best-effort cleanup

	wazaConfig := "files:\n  evalFile: waza-eval.yaml\n  taskGlob: tasks/*.waza-task.yaml\n  taskFileSuffix: .waza-task.yaml\n"
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".waza.yaml"), []byte(wazaConfig), 0o644))

	files := ReadProjectFiles()
	assert.Equal(t, "waza-eval.yaml", files.EvalFile)
	assert.Equal(t, "tasks/*.waza-task.yaml", files.TaskGlob)
	assert.Equal(t, ".waza-task.yaml", files.TaskFileSuffix)
}

func TestEvalYAML_SchemaCompliant(t *testing.T) {
	content := EvalYAML("test-skill", "codex-cli", "gpt-5.5")
	errs := validation.ValidateEvalBytes([]byte(content))
	require.Empty(t, errs, "scaffolded eval.yaml should pass schema validation: %v", errs)
}

func TestTaskFiles_SchemaCompliant(t *testing.T) {
	tasks := TaskFiles("test-skill")
	for name, content := range tasks {
		errs := validation.ValidateTaskBytes([]byte(content))
		require.Empty(t, errs, "scaffolded task %s should pass schema validation: %v", name, errs)
	}
}
