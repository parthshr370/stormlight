package adapter

import "testing"

func TestParseModelSpec(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		provider string
		modelID  string
		baseURL  string
	}{
		{"bare model", "claude-opus-4-8", "", "claude-opus-4-8", ""},
		{"slash model no provider", "anthropic/claude-opus-4-8", "", "anthropic/claude-opus-4-8", ""},
		{"provider prefix", "anthropic:claude-sonnet-4", "anthropic", "claude-sonnet-4", ""},
		{"openai with base url", "openai:gpt-4o@https://api.openai.com/v1", "openai", "gpt-4o", "https://api.openai.com/v1"},
		{"empty", "", "", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := parseModelSpec(tc.raw)
			if got.Provider != tc.provider || got.ModelID != tc.modelID || got.BaseURL != tc.baseURL {
				t.Fatalf("parseModelSpec(%q) = %+v", tc.raw, got)
			}
		})
	}
}

func TestModelIDFromSpec(t *testing.T) {
	tests := map[string]string{
		"claude-opus-4-8":           "claude-opus-4-8",
		"anthropic:claude-sonnet-4": "claude-sonnet-4",
		"anthropic/claude-opus-4-8": "anthropic/claude-opus-4-8",
	}
	for raw, want := range tests {
		if got := ModelIDFromSpec(raw); got != want {
			t.Fatalf("ModelIDFromSpec(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestResolveRouteDefaultsToAnthropic(t *testing.T) {
	rt, err := resolveRoute(RoutingConfig{}, func(string) string { return "" })
	if err != nil {
		t.Fatalf("resolveRoute: %v", err)
	}
	if rt.API != AnthropicMessagesAPI || rt.Model.Provider != "anthropic" || rt.Model.ID != "claude-opus-4-8" {
		t.Fatalf("route = %+v", rt)
	}
}

func TestResolveRouteUnknownProviderErrors(t *testing.T) {
	if _, err := resolveRoute(RoutingConfig{ModelID: "mistral:large"}, func(string) string { return "" }); err == nil {
		t.Fatal("expected error for unknown provider")
	}
}
