package base

import (
	"context"
	"testing"

	"go.harness.dev/harness/internal/engine/stream"
	"go.harness.dev/harness/internal/engine/types"
)

func TestRegistryResolve(t *testing.T) {
	r := NewRegistry()
	p := Provider{ID: "anthropic", Models: []types.Model{{ID: "claude-x", API: "anthropic-messages"}}}
	r.Register("anthropic-messages", p)

	got, ok := r.Resolve("anthropic-messages")
	if !ok || got.ID != "anthropic" {
		t.Fatalf("Resolve(anthropic-messages) = %+v, %v", got, ok)
	}
	if _, ok := r.Resolve("missing-api"); ok {
		t.Fatalf("Resolve(missing-api) unexpectedly found a provider")
	}
}

func TestRegistryRegisterReplaces(t *testing.T) {
	r := NewRegistry()
	r.Register("api", Provider{ID: "first"})
	r.Register("api", Provider{ID: "second"})
	if got, _ := r.Resolve("api"); got.ID != "second" {
		t.Fatalf("re-register did not replace: %q", got.ID)
	}
}

func TestWithEnvAPIKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-from-env")
	t.Setenv("ANTHROPIC_OAUTH_TOKEN", "oauth-from-env")

	// Empty APIKey is filled from the provider's known env var, with Anthropic
	// OAuth taking precedence.
	opts := WithEnvAPIKey(&types.StreamOptions{}, "anthropic")
	if opts.APIKey != "oauth-from-env" {
		t.Fatalf("APIKey = %q, want oauth-from-env", opts.APIKey)
	}

	// An explicit APIKey is preserved (env does not override).
	opts = WithEnvAPIKey(&types.StreamOptions{APIKey: "sk-explicit"}, "anthropic")
	if opts.APIKey != "sk-explicit" {
		t.Fatalf("explicit APIKey overwritten: %q", opts.APIKey)
	}

	// nil opts yields a fresh StreamOptions.
	if got := WithEnvAPIKey(nil, "anthropic"); got == nil || got.APIKey != "oauth-from-env" {
		t.Fatalf("nil opts: got %+v", got)
	}
	// Non-mutating: the caller's opts is not modified in place.
	original := &types.StreamOptions{}
	returned := WithEnvAPIKey(original, "anthropic")
	if original.APIKey != "" {
		t.Fatalf("WithEnvAPIKey mutated the caller's opts: %q", original.APIKey)
	}
	if returned == original {
		t.Fatalf("WithEnvAPIKey returned the same pointer; expected a copy when filling")
	}
	if returned.APIKey != "oauth-from-env" {
		t.Fatalf("returned copy APIKey = %q", returned.APIKey)
	}
	// A whitespace-only key counts as absent.
	if got := WithEnvAPIKey(&types.StreamOptions{APIKey: "   "}, "anthropic"); got.APIKey != "oauth-from-env" {
		t.Fatalf("whitespace key not treated as absent: %q", got.APIKey)
	}
}

func TestWithEnvAPIKeyDerivedVar(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "sk-openai")
	if got := WithEnvAPIKey(&types.StreamOptions{}, "openai"); got.APIKey != "sk-openai" {
		t.Fatalf("derived env var not used: %q", got.APIKey)
	}
}

func TestWithEnvAPIKeyKnownProviderVar(t *testing.T) {
	t.Setenv("GEMINI_API_KEY", "sk-gemini")
	t.Setenv("GOOGLE_API_KEY", "wrong")
	if got := WithEnvAPIKey(&types.StreamOptions{}, "google"); got.APIKey != "sk-gemini" {
		t.Fatalf("google APIKey = %q, want GEMINI_API_KEY", got.APIKey)
	}
}

// The StreamFunc seam composes with stream.AssistantStream end-to-end: a provider
// produces a stream a consumer drains via Events()/Result().
func TestStreamFuncSeam(t *testing.T) {
	final := &types.AssistantMessage{Content: []types.ContentBlock{types.NewText("hi")}}
	var fn StreamFunc = func(ctx context.Context, model types.Model, c types.Context, opts *types.StreamOptions) *stream.AssistantStream {
		s := stream.NewAssistantStream(model.API, model.Provider, model.ID)
		go func() {
			s.Push(types.StreamEvent{Type: types.EvTextStart, ContentIndex: 0})
			s.Push(types.StreamEvent{Type: types.EvTextDelta, ContentIndex: 0, Delta: "hi"})
			s.Push(types.StreamEvent{Type: types.EvDone, Message: final, Reason: types.StopStop})
		}()
		return s
	}

	p := Provider{ID: "faux", Stream: fn}
	s := p.Stream(context.Background(), types.Model{API: "faux-api", Provider: "faux", ID: "m"}, types.Context{}, nil)

	var count int
	for range s.Events() {
		count++
	}
	if count != 3 {
		t.Fatalf("event count = %d, want 3", count)
	}
	got, err := s.Result(context.Background())
	if err != nil || got != final {
		t.Fatalf("Result = %p, %v; want %p", got, err, final)
	}
}
