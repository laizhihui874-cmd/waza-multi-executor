package execution

import "context"

// Tool is an engine-neutral callable tool declaration. Engine adapters map it
// to their native SDK representation when the engine supports prompt tools.
type Tool struct {
	Name                 string
	Description          string
	Parameters           map[string]any
	OverridesBuiltInTool bool
	SkipPermission       bool
	Defer                string
	Metadata             map[string]any
	Handler              ToolHandler
}

type ToolHandler func(ToolInvocation) (ToolResult, error)

type ToolInvocation struct {
	SessionID      string
	ToolCallID     string
	ToolName       string
	Arguments      any
	AvailableTools any
	TraceContext   context.Context
}

type ToolResult struct {
	TextResultForLLM string
	ResultType       string
}

type PermissionAction string

const (
	PermissionApproveOnce      PermissionAction = "approve_once"
	PermissionReject           PermissionAction = "reject"
	PermissionUserNotAvailable PermissionAction = "user_not_available"
	PermissionNoResult         PermissionAction = "no_result"
)

type PermissionRequest struct {
	Raw any
}

type PermissionInvocation struct {
	SessionID string
}

type PermissionDecision struct {
	Action   PermissionAction
	Feedback string
}

type PermissionHandlerFunc func(PermissionRequest, PermissionInvocation) (PermissionDecision, error)

func ApproveAllPermissions(PermissionRequest, PermissionInvocation) (PermissionDecision, error) {
	return PermissionDecision{Action: PermissionApproveOnce}, nil
}
