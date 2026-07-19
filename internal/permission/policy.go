package permission

import (
	"context"
	"fmt"
	"strings"
)

// Tier is a tool's approval tier. Unknown tools are [TierExec].
type Tier int

const (
	// TierRead allows tools that inspect existing data.
	TierRead Tier = iota
	// TierWrite allows tools that change workspace content.
	TierWrite
	// TierExec allows tools that may execute arbitrary actions.
	TierExec
)

// Decision is a per-tool user policy outcome.
type Decision int

const (
	// DecideAllow allows a tool invocation.
	DecideAllow Decision = iota
	// DecideDeny denies a tool invocation.
	DecideDeny
	// DecidePrompt requires interactive approval for a tool invocation.
	DecidePrompt
)

// PromptRequest describes an approval prompt for a tool call.
type PromptRequest struct {
	// Tool names the tool requesting approval.
	Tool string
	// Tier is the approval tier for Tool.
	Tier Tier
	// Reason provides a safety-override reason when one applies.
	Reason string
}

// Prompter asks a human to approve a tool call.
type Prompter interface {
	// Approve reports whether req is approved.
	Approve(ctx context.Context, req PromptRequest) (bool, error)
}

// Policy is the per-session approval policy layered over the mode.
type Policy struct {
	// Overrides stores per-tool allow, deny, or prompt decisions.
	Overrides map[string]Decision
	// Prompter resolves prompt decisions; nil denies them in headless sessions.
	Prompter Prompter
}

// ClassifyTool returns the approval tier for toolName.
func ClassifyTool(toolName string) Tier {
	switch toolName {
	case "read", "grep", "find", "ls", "web_search", "web_fetch", "attachment", "skill":
		return TierRead
	case "write", "edit", "todo_write":
		return TierWrite
	default:
		return TierExec
	}
}

// ParseDecision converts an approval settings value into a [Decision].
func ParseDecision(value string) (Decision, error) {
	switch strings.TrimSpace(value) {
	case "allow":
		return DecideAllow, nil
	case "deny":
		return DecideDeny, nil
	case "prompt":
		return DecidePrompt, nil
	default:
		return DecideDeny, fmt.Errorf("unknown permission decision %q", value)
	}
}
