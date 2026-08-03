package execution

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/microsoft/waza/internal/models"
)

// EngineCapabilities describes optional evaluation features surfaced by an
// executor. Core final-output and workspace grading are supported by every
// registered engine and therefore do not need capability flags.
type EngineCapabilities struct {
	ToolEvents       bool
	Usage            bool
	MCP              bool
	SkillInvocations bool
	Sessions         bool
	PromptTools      bool
}

// EngineDescriptor is stable metadata for a registered executor.
type EngineDescriptor struct {
	Name         string
	Capabilities EngineCapabilities
}

// EngineConfig is supplied to an EngineFactory for one evaluation run.
type EngineConfig struct {
	ModelID string
	Options models.ExecutorConfig
}

// EngineFactory constructs an AgentEngine without initializing it.
type EngineFactory func(EngineConfig) (AgentEngine, error)

type engineRegistration struct {
	descriptor EngineDescriptor
	factory    EngineFactory
}

// EngineRegistry owns the product-visible executor names and factories.
// Registration is concurrency-safe so tests and embedders can construct a
// private registry without mutating package-global state.
type EngineRegistry struct {
	mu      sync.RWMutex
	engines map[string]engineRegistration
}

func NewEngineRegistry() *EngineRegistry {
	return &EngineRegistry{engines: make(map[string]engineRegistration)}
}

func (r *EngineRegistry) Register(name string, capabilities EngineCapabilities, factory EngineFactory) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("executor name is required")
	}
	if factory == nil {
		return fmt.Errorf("executor %q factory is required", name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.engines[name]; exists {
		return fmt.Errorf("executor %q is already registered", name)
	}
	r.engines[name] = engineRegistration{
		descriptor: EngineDescriptor{Name: name, Capabilities: capabilities},
		factory:    factory,
	}
	return nil
}

func (r *EngineRegistry) Create(name string, cfg EngineConfig) (AgentEngine, EngineDescriptor, error) {
	r.mu.RLock()
	registration, ok := r.engines[name]
	r.mu.RUnlock()
	if !ok {
		return nil, EngineDescriptor{}, fmt.Errorf("unknown executor %q (supported: %s)", name, strings.Join(r.Names(), ", "))
	}
	engine, err := registration.factory(cfg)
	if err != nil {
		return nil, registration.descriptor, fmt.Errorf("create executor %q: %w", name, err)
	}
	if engine == nil {
		return nil, registration.descriptor, fmt.Errorf("create executor %q: factory returned nil engine", name)
	}
	return engine, registration.descriptor, nil
}

func (r *EngineRegistry) Descriptor(name string) (EngineDescriptor, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	registration, ok := r.engines[name]
	return registration.descriptor, ok
}

func (r *EngineRegistry) Names() []string {
	r.mu.RLock()
	names := make([]string, 0, len(r.engines))
	for name := range r.engines {
		names = append(names, name)
	}
	r.mu.RUnlock()
	sort.Strings(names)
	return names
}

// ValidateCapabilities rejects configurations that require evidence an engine
// cannot produce. It never silently disables graders or MCP servers.
func ValidateCapabilities(spec *models.EvalSpec, descriptor EngineDescriptor) error {
	if spec == nil {
		return fmt.Errorf("validate executor capabilities: eval spec is nil")
	}
	missing := make(map[string]struct{})
	if (len(spec.Config.ServerConfigs) > 0 || len(spec.MCPMocks) > 0) && !descriptor.Capabilities.MCP {
		missing["mcp"] = struct{}{}
	}
	for _, grader := range spec.Graders {
		addGraderCapabilityRequirement(missing, grader.Kind, descriptor.Capabilities)
	}
	return unsupportedCapabilitiesError(descriptor.Name, missing)
}

// ValidateTestCaseCapabilities checks task-level and checkpoint graders plus
// multi-turn features. This complements ValidateCapabilities, which handles
// the eval-level configuration.
func ValidateTestCaseCapabilities(testCases []*models.TestCase, descriptor EngineDescriptor) error {
	missing := make(map[string]struct{})
	for _, testCase := range testCases {
		if testCase == nil {
			continue
		}
		for _, grader := range testCase.Validators {
			addGraderCapabilityRequirement(missing, grader.Kind, descriptor.Capabilities)
		}
		for _, checkpoint := range testCase.Checkpoints {
			for _, grader := range checkpoint.Graders {
				addGraderCapabilityRequirement(missing, grader.Kind, descriptor.Capabilities)
			}
		}
		if (len(testCase.Stimulus.FollowUps) > 0 || testCase.Stimulus.Responder != nil) && !descriptor.Capabilities.Sessions {
			missing["sessions"] = struct{}{}
		}
		if testCase.Stimulus.Responder != nil && !descriptor.Capabilities.PromptTools {
			missing["prompt tools"] = struct{}{}
		}
	}
	return unsupportedCapabilitiesError(descriptor.Name, missing)
}

func addGraderCapabilityRequirement(missing map[string]struct{}, kind models.GraderKind, capabilities EngineCapabilities) {
	switch kind {
	case models.GraderKindPrompt:
		if !capabilities.PromptTools {
			missing["prompt tools"] = struct{}{}
		}
	case models.GraderKindBehavior, models.GraderKindActionSequence,
		models.GraderKindToolConstraint, models.GraderKindToolCalls:
		if !capabilities.ToolEvents {
			missing["tool events"] = struct{}{}
		}
	case models.GraderKindSkillInvocation, models.GraderKindTrigger:
		if !capabilities.SkillInvocations {
			missing["skill invocation events"] = struct{}{}
		}
	}
}

func unsupportedCapabilitiesError(executorName string, missing map[string]struct{}) error {
	if len(missing) == 0 {
		return nil
	}
	features := make([]string, 0, len(missing))
	for feature := range missing {
		features = append(features, feature)
	}
	sort.Strings(features)
	return fmt.Errorf("executor %q does not support required capabilities: %s", executorName, strings.Join(features, ", "))
}
