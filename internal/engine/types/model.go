package types

import "slices"

// AnthropicCompat holds per-model Anthropic compatibility flags. Pointers keep
// the tri-state (unset vs explicit true/false) so a nil default can be resolved
// later (02-map-ai-agent.md A17).
type AnthropicCompat struct {
	SupportsEagerToolInputStreaming *bool `json:"supportsEagerToolInputStreaming,omitempty"`
	SupportsLongCacheRetention      *bool `json:"supportsLongCacheRetention,omitempty"`
	SendSessionAffinityHeaders      *bool `json:"sendSessionAffinityHeaders,omitempty"`
	SupportsCacheControlOnTools     *bool `json:"supportsCacheControlOnTools,omitempty"`
	SupportsTemperature             *bool `json:"supportsTemperature,omitempty"`
	ForceAdaptiveThinking           *bool `json:"forceAdaptiveThinking,omitempty"`
	AllowEmptySignature             *bool `json:"allowEmptySignature,omitempty"`
}

// Model describes an LLM plus capability metadata (02-map-ai-agent.md A16). The
// generic Model<TApi> collapses to a concrete struct; we only carry the
// Anthropic compat we need.
type Model struct {
	ID               string             `json:"id"`
	Name             string             `json:"name"`
	API              string             `json:"api"`
	Provider         string             `json:"provider"`
	BaseURL          string             `json:"baseUrl,omitempty"`
	Reasoning        bool               `json:"reasoning,omitempty"`
	ThinkingLevelMap map[string]*string `json:"thinkingLevelMap,omitempty"`
	Input            []string           `json:"input,omitempty"`
	Cost             ModelCost          `json:"cost"`
	ContextWindow    int                `json:"contextWindow,omitempty"`
	MaxTokens        int                `json:"maxTokens,omitempty"`
	Headers          map[string]string  `json:"headers,omitempty"`
	Compat           *AnthropicCompat   `json:"compat,omitempty"`
}

// SupportsVision reports whether the model accepts image input.
func (m Model) SupportsVision() bool {
	return slices.Contains(m.Input, "image")
}
