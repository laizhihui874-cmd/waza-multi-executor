package execution

import (
	"context"

	copilot "github.com/github/copilot-sdk/go"
	"github.com/github/copilot-sdk/go/rpc"
)

func copilotTools(tools []Tool) []copilot.Tool {
	if len(tools) == 0 {
		return nil
	}
	result := make([]copilot.Tool, 0, len(tools))
	for _, tool := range tools {
		tool := tool
		converted := copilot.Tool{
			Name: tool.Name, Description: tool.Description, Parameters: tool.Parameters,
			OverridesBuiltInTool: tool.OverridesBuiltInTool, SkipPermission: tool.SkipPermission,
			Defer: copilot.ToolDefer(tool.Defer), Metadata: tool.Metadata,
		}
		if tool.Handler != nil {
			converted.Handler = func(invocation copilot.ToolInvocation) (copilot.ToolResult, error) {
				traceContext := invocation.TraceContext
				if traceContext == nil {
					traceContext = context.Background()
				}
				value, err := tool.Handler(ToolInvocation{
					SessionID: invocation.SessionID, ToolCallID: invocation.ToolCallID,
					ToolName: invocation.ToolName, Arguments: invocation.Arguments,
					AvailableTools: invocation.AvailableTools, TraceContext: traceContext,
				})
				return copilot.ToolResult{TextResultForLLM: value.TextResultForLLM, ResultType: value.ResultType}, err
			}
		}
		result = append(result, converted)
	}
	return result
}

func copilotPermissionHandler(handler PermissionHandlerFunc) copilot.PermissionHandlerFunc {
	if handler == nil {
		return allowAllTools
	}
	return func(request copilot.PermissionRequest, invocation copilot.PermissionInvocation) (rpc.PermissionDecision, error) {
		decision, err := handler(PermissionRequest{Raw: request}, PermissionInvocation{SessionID: invocation.SessionID})
		if err != nil {
			return nil, err
		}
		switch decision.Action {
		case PermissionApproveOnce:
			return &rpc.PermissionDecisionApproveOnce{}, nil
		case PermissionReject:
			feedback := decision.Feedback
			return &rpc.PermissionDecisionReject{Feedback: &feedback}, nil
		case PermissionNoResult:
			return &rpc.PermissionDecisionNoResult{}, nil
		case PermissionUserNotAvailable:
			return &rpc.PermissionDecisionUserNotAvailable{}, nil
		default:
			return &rpc.PermissionDecisionUserNotAvailable{}, nil
		}
	}
}
