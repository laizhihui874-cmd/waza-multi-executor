package execution

import (
	"context"
	"errors"
	"testing"

	"github.com/microsoft/waza/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type registryTestEngine struct{}

func (*registryTestEngine) Initialize(context.Context) error { return nil }
func (*registryTestEngine) Execute(context.Context, *ExecutionRequest) (*ExecutionResponse, error) {
	return &ExecutionResponse{Success: true}, nil
}
func (*registryTestEngine) Shutdown(context.Context) error         { return nil }
func (*registryTestEngine) SessionUsage(string) *models.UsageStats { return nil }

func TestEngineRegistry_RegisterAndCreate(t *testing.T) {
	registry := NewEngineRegistry()
	caps := EngineCapabilities{Usage: true, ToolEvents: true}
	err := registry.Register("test-cli", caps, func(cfg EngineConfig) (AgentEngine, error) {
		assert.Equal(t, "test-model", cfg.ModelID)
		return &registryTestEngine{}, nil
	})
	require.NoError(t, err)

	engine, descriptor, err := registry.Create("test-cli", EngineConfig{ModelID: "test-model"})
	require.NoError(t, err)
	assert.IsType(t, &registryTestEngine{}, engine)
	assert.Equal(t, "test-cli", descriptor.Name)
	assert.Equal(t, caps, descriptor.Capabilities)
	gotDescriptor, ok := registry.Descriptor("test-cli")
	assert.True(t, ok)
	assert.Equal(t, descriptor, gotDescriptor)
	_, ok = registry.Descriptor("missing")
	assert.False(t, ok)
}

func TestEngineRegistry_RejectsInvalidRegistrations(t *testing.T) {
	registry := NewEngineRegistry()
	factory := func(EngineConfig) (AgentEngine, error) { return &registryTestEngine{}, nil }

	require.Error(t, registry.Register("", EngineCapabilities{}, factory))
	require.NoError(t, registry.Register("valid", EngineCapabilities{}, factory))
	require.Error(t, registry.Register("valid", EngineCapabilities{}, factory))
	require.Error(t, registry.Register("nil-factory", EngineCapabilities{}, nil))
}

func TestEngineRegistry_UnknownExecutorListsSupportedNames(t *testing.T) {
	registry := NewEngineRegistry()
	require.NoError(t, registry.Register("codex-cli", EngineCapabilities{}, func(EngineConfig) (AgentEngine, error) {
		return &registryTestEngine{}, nil
	}))

	_, _, err := registry.Create("missing", EngineConfig{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown executor")
	assert.Contains(t, err.Error(), "codex-cli")
}

func TestEngineRegistry_SurfacesFactoryErrorsAndNilEngines(t *testing.T) {
	registry := NewEngineRegistry()
	require.NoError(t, registry.Register("error", EngineCapabilities{}, func(EngineConfig) (AgentEngine, error) {
		return nil, errors.New("factory failed")
	}))
	require.NoError(t, registry.Register("nil", EngineCapabilities{}, func(EngineConfig) (AgentEngine, error) {
		return nil, nil
	}))
	_, _, err := registry.Create("error", EngineConfig{})
	require.ErrorContains(t, err, "factory failed")
	_, _, err = registry.Create("nil", EngineConfig{})
	require.ErrorContains(t, err, "nil engine")
}

func TestApproveAllPermissions(t *testing.T) {
	decision, err := ApproveAllPermissions(PermissionRequest{}, PermissionInvocation{})
	require.NoError(t, err)
	assert.Equal(t, PermissionApproveOnce, decision.Action)
}

func TestValidateCapabilitiesRejectsUnsupportedAdvancedFeatures(t *testing.T) {
	spec := &models.EvalSpec{
		Config:  models.Config{ServerConfigs: map[string]any{"demo": map[string]any{"type": "stdio"}}},
		Graders: []models.GraderConfig{{Kind: models.GraderKindPrompt, Identifier: "judge"}},
	}

	err := ValidateCapabilities(spec, EngineDescriptor{Name: "generic-cli"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "generic-cli")
	assert.Contains(t, err.Error(), "mcp")
	assert.Contains(t, err.Error(), "prompt tools")
}

func TestValidateCapabilitiesAllowsCoreGraders(t *testing.T) {
	spec := &models.EvalSpec{
		Graders: []models.GraderConfig{
			{Kind: models.GraderKindText, Identifier: "text"},
			{Kind: models.GraderKindFile, Identifier: "files"},
		},
	}

	require.NoError(t, ValidateCapabilities(spec, EngineDescriptor{Name: "generic-cli"}))
}

func TestValidateTestCaseCapabilitiesChecksTaskCheckpointAndMultiTurnFeatures(t *testing.T) {
	testCases := []*models.TestCase{{
		Validators:  []models.ValidatorInline{{Kind: models.GraderKindBehavior}},
		Checkpoints: []models.Checkpoint{{Graders: []models.ValidatorInline{{Kind: models.GraderKindSkillInvocation}}}},
		Stimulus: models.TaskStimulus{
			FollowUps: []string{"continue"},
			Responder: &models.ResponderConfig{Instructions: "answer", MaxFollowups: 1},
		},
	}}

	err := ValidateTestCaseCapabilities(testCases, EngineDescriptor{Name: "generic-cli"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tool events")
	assert.Contains(t, err.Error(), "skill invocation events")
	assert.Contains(t, err.Error(), "sessions")
	assert.Contains(t, err.Error(), "prompt tools")

	require.NoError(t, ValidateTestCaseCapabilities(testCases, EngineDescriptor{
		Name: "full", Capabilities: EngineCapabilities{
			ToolEvents: true, SkillInvocations: true, Sessions: true, PromptTools: true,
		},
	}))
}
