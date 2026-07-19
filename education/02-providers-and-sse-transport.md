# 02 — Providers and SSE transport

## The problem

The harness talks to language models over HTTP streaming APIs. Each API speaks a
different dialect. Anthropic uses the Messages protocol with six SSE event types
(`message_start`, `content_block_start`, `content_block_delta`,
`content_block_stop`, `message_delta`, `message_stop`). OpenAI chat completions
streams use a flatter chunk model with an index/id pair for tracking tool calls.
Google Gemini emits `generateContent` SSE frames where function calls arrive as
inline parts rather than progressive deltas.

Without a provider abstraction, the agent loop would need to know about every
API's wire format, auth scheme, base URL, thinking configuration, and streaming
semantics. That means three or four switch-on-provider branches scattered through
request construction, response parsing, error classification, and retry
classification. Adding a new provider becomes a surgery across five packages.

Three concrete problems the abstraction solves:

1. **Deterministic testing.** The agent loop, retry decorator, compaction,
   and prompt builder all need a stream of assistant events to exercise their
   logic. You cannot depend on a live API for unit tests — latency,
   non-determinism, and network failures make them flaky. The `faux` provider
   replaces a real API with a scripted queue of `ResponseStep`s, seeded
   chunking for realistic delta-by-delta output, and zero network I/O.

2. **Model portability.** A configuration change from
   `claude-opus-4-8` to `openai:gpt-4o@https://api.openai.com/v1` should not
   require code changes in the agent loop, session manager, or retry logic.
   The registry dispatches by API identifier; the loop only sees
   `stream.AssistantStream`.

3. **Stream health.** A hung SSE connection — where the server accepted the
   request but never sends another event — pins a goroutine and a CPU core.
   The idle watchdog in every provider's `runStream` guards against this wedge
   with a configurable timeout (`HARNESS_STREAM_IDLE_SECONDS`, default 90s).

## Key decisions and the thinking process

### The Provider struct is a bundle, not an interface

```go
// internal/provider/base/provider.go:25-30
type Provider struct {
    ID           string
    Models       []types.Model
    Stream       StreamFunc
    StreamSimple SimpleStreamFunc
}
```

The natural Go instinct is `type Provider interface { Stream(...) ... }`. We
chose a struct carrying function fields instead.

The decision turns on registration. `NewProviderRouter` builds a `Registry`,
registers all known providers (Anthropic, OpenAI, Google), then resolves the
configured route. A `Provider` interface with four implementations would still
need a registry — you can't resolve `"anthropic-messages"` → implementation
dynamically without a map. And a struct carrying function fields means the
registry is a plain `map[string]Provider` that the adapter package wires up
at construction time. No init-time side effects, no global state, no `import
_ "..."` for registration.

The `Stream` and `StreamSimple` fields are both present because the adapter
needs two levels: the low-level `StreamFunc` (Anthropic's reasoning effort,
thinking budget) and `SimpleStreamFunc` (the reasoning-level string that
the agent loop uses). The `SimpleStreamFunc` is what gets wired into
`agent.StreamFn`; the raw `StreamFunc` is the escape hatch for provider-aware
callers.

### Unknown providers are a hard config error, never a silent fallback

`resolveRoute` at `internal/adapter/modelspec.go:94-131` has a `switch prov`
with cases for `"anthropic"` and `"openai"` and a `default` that returns a
deterministic error:

```go
default:
    return route{}, fmt.Errorf("unsupported model provider; use anthropic or openai")
```

There is no fallback to Anthropic. If you type `mistral:large`, the harness
refuses to start. The alternative — silently treating unknown providers as
Anthropic — would mean a typo in a config file routes your requests to the
wrong API with the wrong auth. A `mistral:large` spec would hit the Anthropic
endpoint with an Anthropic key and a model name that does not exist there,
producing a confusing 404 or 400 from the API rather than a clear
"unsupported provider" at startup.

`ValidateModelSpec` at `modelspec.go:52-66` enforces this early, before
any HTTP call is made. The validation runs during config resolution, not at
stream time, so a misconfigured harness never starts a session.

### Why faux exists

`internal/provider/faux/` is a scripted in-memory provider that implements the
`base.Provider` contract. It maintains a queue of `ResponseStep`s — functions
that receive the current call's `types.Context`, `types.StreamOptions`,
`State` (mutable snapshot with call count), and `types.Model`, and return an
`types.AssistantMessage` plus an optional error.

The critical design choice: faux replays responses as progressive
`StreamEvent`s (start, text_delta, text_end, done), not as a single
pre-assembled message. This matters because the agent loop, retry decorator,
and compaction all observe and react to events one at a time. A
single-message faux would not catch bugs where code misinterprets event
ordering, ignores a partial update, or fails to handle a mid-stream error.

The seeded chunking (`produce` at `faux.go:248-273`) splits text into
token-sized fragments with deterministic randomness, so two runs with the
same seed produce identical event sequences. This makes `faux` usable in
table-driven tests that assert exact event traces.

### OnResponse is a hook, not a post-stream callback

`types.StreamOptions.OnResponse` is `func(ProviderResponse, Model) error`. It
fires immediately after `http.DefaultClient.Do` returns, before any SSE bytes
are read. This timing is deliberate: the hook can inspect the HTTP status code
and response headers and return an error that terminates the stream *before*
the SSE parser runs.

The retry decorator (module 06) depends on this hook. It installs an
`OnResponse` that classifies status codes (429 → rate-limited and retryable,
5xx → server error, 4xx → client error) and records response headers that
inform backoff. If `OnResponse` were a post-stream callback, retry
classification would need to buffer the entire response body, parse it, and
then decide — that loses the ability to fail fast on a non-streamable status.

All three providers call `OnResponse` at the same point in `runStream`:

- Anthropic: `stream.go:182-186`
- OpenAI: `stream.go:126-130`
- Google: `stream.go:130-134`

Each passes a `types.ProviderResponse` with `Status` (the HTTP status code)
and `Headers` (a `map[string]string` built from `util.HeadersToRecord`).

### Credential precedence

A model route carries credentials from four possible sources, resolved in
`resolveRoute` and layered by `withStream`:

1. **Config struct** (`RoutingConfig.APIKey`, `RoutingConfig.AuthToken`) —
   set explicitly by the embedder.
2. **Per-provider env vars** — `ANTHROPIC_OAUTH_TOKEN` then
   `ANTHROPIC_API_KEY` for Anthropic; `GEMINI_API_KEY` for Google.
3. **Derived env var** — for unlisted providers, `<UPPERCASED>_API_KEY`
   (e.g. `OPENAI_API_KEY`).
4. **Auth token fallback** — if no API key is set but `ANTHROPIC_AUTH_TOKEN`
   is available, `withStream` injects a `Bearer <token>` authorization header
   (`provider.go:132-137`).

The explicit config always wins. `WithEnvAPIKey` at `base/provider.go:92-111`
fills a blank API key from the environment but preserves a caller-supplied
key — and it returns a shallow copy so it never mutates the caller's struct.

## Signatures and types

### The base provider contract

```go
// internal/provider/base/provider.go:18
type StreamFunc func(ctx context.Context, model types.Model, c types.Context,
    opts *types.StreamOptions) *stream.AssistantStream
```
- `ctx` — cancellation from the agent loop. A derived `streamCtx` is created
  inside `runStream`; it is the watchdog's cancel target, not `ctx` directly.
- `model` — the resolved `types.Model` with API, provider, base URL, max
  tokens, context window, and compat flags all filled.
- `c` — the conversation context: system prompt, messages, tools.
- `opts` — per-request options carrying API key, temperature, headers, max
  tokens, caching, `OnResponse`, `OnPayload`, and timeout.
- Returns an `AssistantStream` — the caller ranges over `Events()` or calls
  `Result()`. Errors never come through the return value; they arrive as an
  `EvError` stream event followed by stream close.

```go
// internal/provider/base/provider.go:22
type SimpleStreamFunc func(ctx context.Context, model types.Model, c types.Context,
    opts *types.SimpleStreamOptions) *stream.AssistantStream
```
- `opts` — adds `Reasoning` (`minimal|low|medium|high|xhigh`) and
  `ThinkingBudgets` on top of `StreamOptions`. Each provider maps the
  reasoning level to its own thinking config before calling `Stream`.

```go
// internal/provider/base/provider.go:32-35
type Registry struct { byAPI map[string]Provider }
func NewRegistry() *Registry
func (r *Registry) Register(api string, p Provider)
func (r *Registry) Resolve(api string) (Provider, bool)
```
- `api` — the API protocol identifier (`"anthropic-messages"`,
  `"openai-completions"`, `"google-generative-ai"`). This is not the
  human-facing provider name (`"anthropic"`); it ties to the wire protocol.

```go
// internal/provider/base/provider.go:92
func WithEnvAPIKey(opts *types.StreamOptions, provider string) *types.StreamOptions
```
- `opts` — may be `nil` (returns a fresh `StreamOptions` with the env key).
- `provider` — the provider id string used to look up the env var name.
- Returns the original pointer when the key is already set (no-op) or the
  env is empty; returns a shallow copy when the env provides a key. Never
  mutates the caller's struct.

### Model spec parsing and routing

```go
// internal/adapter/modelspec.go:14-18
type modelSpec struct {
    Provider string  // text before first ':'
    ModelID  string  // text between ':' and '@', or whole string
    BaseURL  string  // text after '@', or ""
}
```

```go
// internal/adapter/modelspec.go:26
func parseModelSpec(raw string) modelSpec
```
- `raw` — `"claude-opus-4-8"`, `"anthropic:claude-sonnet-4"`,
  `"openai:gpt-4o@https://api.openai.com/v1"`. Trims whitespace; lowercases
  the provider. A bare name with no `:` yields an empty `Provider` (defaults
  to Anthropic in `resolveRoute`).

```go
// internal/adapter/modelspec.go:46
func ModelIDFromSpec(raw string) string
```

```go
// internal/adapter/modelspec.go:52
func ValidateModelSpec(raw string) error
```
- Rejects empty model ids, unsupported providers, and URLs with userinfo,
  query strings, or fragments. Called at config resolution time.

```go
// internal/adapter/modelspec.go:82-87
type route struct {
    Model     types.Model
    API       string
    APIKey    string
    AuthToken string
}
```

```go
// internal/adapter/modelspec.go:94
func resolveRoute(cfg RoutingConfig, env func(string) string) (route, error)
```
- `cfg` — optional `ModelID`, `BaseURL`, provider-specific base URLs,
  `APIKey`, `AuthToken`, `MaxTokens`. Every field is optional; missing values
  flow through `env` lookups and defaults.
- `env` — a `func(string) string` (usually `os.Getenv`). This indirection
  keeps credential resolution testable without mutating the process
  environment.
- Returns the fully resolved `route` or an error for unknown providers.

```go
// internal/adapter/provider.go:55-66
type RoutingConfig struct {
    ModelID, BaseURL, AnthropicBaseURL, OpenAIBaseURL string
    APIKey, AuthToken                                 string
    MaxTokens                                         int
    Env                                               func(string) string
    BlobReader                                        types.BlobReader
    Retry                                             *retry.Config
}
```

```go
// internal/adapter/provider.go:70-74
type ProviderRouter struct {
    Registry *base.Registry
    Model    types.Model
    StreamFn agent.StreamFn
}
```

```go
// internal/adapter/provider.go:83
func NewProviderRouter(cfg RoutingConfig) (ProviderRouter, error)
```
- Builds the registry, registers all known providers, resolves the route,
  wires `StreamFn` through credential injection and optional retry wrapping.

```go
// internal/adapter/roles.go:31
func ResolveRoles(cfg RoleRoutingConfig) (RoleRouters, error)
```
- Routes the default, plan, and smol models through `NewProviderRouter`.
  Empty plan/smol models inherit the default model. Returns three independent
  `ProviderRouter` values, one per agent role.

### SSE parser

```go
// internal/transport/sseparse/sse.go:18-22
type ServerSentEvent struct {
    Event string   // "event:" field value
    Data  string   // joined "data:" values, "\n"-delimited
    Raw   []string // verbatim lines composing this event
}
```

```go
// internal/transport/sseparse/sse.go:109
func IterateSSE(ctx context.Context, r io.Reader, fn func(ServerSentEvent) error) error
```
- `ctx` — checked at each read iteration. Cancellation stops the loop.
- `r` — the response body. Read in 4096-byte chunks.
- `fn` — called once per complete event (a blank line triggers flush). Return
  an error to abort iteration.
- Handles `\r\n` and `\n` line endings. Lines starting with `:` are
  comments and silently dropped. A trailing partial line (no line break) at
  EOF is flushed via `decodeSseLine`.

### Stream options with OnResponse

```go
// internal/engine/types/options.go:4-7
type ProviderResponse struct {
    Status  int               `json:"status"`
    Headers map[string]string `json:"headers"`
}
```

```go
// internal/engine/types/options.go:20-42
type StreamOptions struct {
    Temperature, MaxTokens, APIKey, Transport string-like fields
    Headers                                   map[string]string
    Thinking                                  ThinkingConfig
    SessionID, CacheRetention                 string
    OnPayload  func(any, Model) (any, error)        `json:"-"`
    OnResponse func(ProviderResponse, Model) error   `json:"-"`
    TimeoutMs, MaxRetries, MaxRetryDelayMs    int
    Metadata                                  map[string]any
    Env                                       map[string]string
}
```
- `OnResponse` — called after the HTTP response arrives, before SSE reading.
  The hook can return an error to abort the stream. This is where retry
  classification lives.
- `OnPayload` — called before `json.Marshal` of the request body. Can
  transform the payload or reject the request.
- Both are tagged `json:"-"` — they are Go-only hooks, never serialized.

### Faux provider

```go
// internal/provider/faux/faux.go:67
type ResponseStep func(c types.Context, opts *types.StreamOptions,
    state State, model types.Model) (types.AssistantMessage, error)
```

```go
// internal/provider/faux/faux.go:69-85
type Faux struct {
    mu       sync.Mutex
    steps    []ResponseStep
    calls    int
    cache    map[string]promptCacheEntry
    toolID   int64
    rng      *rand.Rand
    models   []types.Model
    seed     int64
}
```

```go
// internal/provider/faux/faux.go:88
func New(opts Options) *Faux
func (f *Faux) SetResponses(steps ...ResponseStep)
func (f *Faux) AppendResponses(steps ...ResponseStep)
func (f *Faux) Provider() base.Provider
func (f *Faux) Stream(ctx context.Context, model types.Model,
    c types.Context, opts *types.StreamOptions) *stream.AssistantStream
```

- `Stream` dequeues the next `ResponseStep`, calls it synchronously to get
  the `AssistantMessage`, then replays it as progressive `StreamEvent`s on a
  goroutine. Seeded chunking (`splitStringByTokenSize`) splits text and
  thinking blocks into 1-4 token fragments with deterministic randomness.

- `Provider()` wraps `Stream` and `StreamSimple` into a `base.Provider` so
  faux plugs into the exact same registry as a real provider. Tests that use
  `provider_test.go`'s `StreamFunc` seam can swap a live provider for a faux
  without touching the agent loop.

## How it works

### 1. Model spec → resolved route

A model string like `"openai:gpt-4o@https://api.openai.com/v1"` enters
`parseModelSpec`:

```
raw = "openai:gpt-4o@https://api.openai.com/v1"
       └──prov──┘ └─rest──┘

prov = "openai", rest = "gpt-4o@https://api.openai.com/v1"
modelID = "gpt-4o", baseURL = "https://api.openai.com/v1"
```

`resolveRoute` receives the parsed spec and the routing config. For
`prov == "openai"`:

1. Creates `types.Model` with `API = "openai-completions"`,
   `Provider = "openai"`, `BaseURL` from spec > config > env > default,
   `ContextWindow = 128000`.
2. Resolves `APIKey` from `cfg.APIKey` > `OPENAI_API_KEY`.
3. Returns `route{Model, API: "openai-completions", APIKey: ...}`.

For a bare model like `"claude-opus-4-8"` (no `:`), `prov` is empty and
defaults to `"anthropic"` with `API = "anthropic-messages"`,
`ContextWindow = 200000`.

### 2. Registry construction

`NewProviderRouter` creates a fresh `Registry`, registers all three
providers (Anthropic, OpenAI, Google) with their `StreamSimple` functions,
resolves the route, and checks:

```go
p, ok := registry.Resolve(rt.API)
if !ok || p.StreamSimple == nil {
    return ProviderRouter{}, fmt.Errorf("no provider registered for model API %q", rt.API)
}
```

The `Registry.Register` call for each API is explicit and happens in
`NewProviderRouter`, not via `init()`. This keeps registration order
visible and under the caller's control.

For the resolved route's API, the matching provider's `StreamSimple` is
bound to `agent.StreamFn` through `withStream`, which injects the API key
or auth token header. If a retry config is present, the stream function is
wrapped in a retry decorator before being stored on `ProviderRouter.StreamFn`.

### 3. Stream lifecycle (Anthropic as the canonical example)

`Stream` at `anthropic/stream.go:36-40` creates an `AssistantStream`, then
launches `runStream` as a goroutine:

```
Stream()
  └─ go runStream(parent, s, model, c, opts)
       ├─ newOutputMessage(model)          // seed the accumulator
       ├─ context.WithCancel(parent)       // streamCtx = watchdog target
       ├─ go watchIdle(done, activity, cancel, &idleFired)
       ├─ assertRequestAuth(...)           // fail fast: no key = error event
       ├─ buildRequestHeaders(...) + buildParams(...)
       ├─ OnPayload(payload, model)        // optional transform
       ├─ json.Marshal(payload)
       ├─ http.NewRequestWithContext(streamCtx, POST, messagesURL, body)
       ├─ http.DefaultClient.Do(req)
       │    └─ OnResponse(resp.StatusCode, resp.Headers, model)
       │         └─ retry classifier inspects 429/5xx/4xx
       ├─ check status < 200 || >= 300 → read body, push error
       ├─ s.Push(EvStart, Partial: output)
       ├─ iterateAnthropicEvents(streamCtx, resp.Body, onActivity, foldFn)
       │    └─ sseparse.IterateSSE(ctx, resp.Body, callback)
       │         ├─ read 4096-byte chunks
       │         ├─ drainCompleteLines → decodeSseLine → decodeSseLine → ...
       │         │    └─ blank line → flushSseEvent → callback(event)
       │         └─ callback:
       │              ├─ onActivity() → non-blocking send to watchdog
       │              ├─ skip non-message events (ping, etc.)
       │              ├─ jsonrepair.ParseJsonWithRepair(event.Data, &raw)
       │              └─ foldAnthropicEvent(raw, state, model)
       │                   └─ foldContentBlockStart / Delta / Stop
       │                        → s.Push(EvTextDelta / EvToolCallStart / ...)
       ├─ enforce message_start→message_stop bracketing
       ├─ streamError(parent.Err(), err, idleFired.Load())
       ├─ check parent.Err() → aborted
       ├─ check output.StopReason → aborted/error
       └─ s.Push(EvDone, Reason, Message)
```

### 4. SSE parsing details

`IterateSSE` at `sseparse/sse.go:109-154` reads in 4096-byte chunks. Each
read appends to a buffer, then `drainCompleteLines` extracts every complete
line (found by `\r\n` or `\n`) and passes it to `decodeSseLine`.

`decodeSseLine` accumulates `event:` and `data:` fields in a `decoderState`.
A blank line triggers `flushSseEvent`, which joins accumulated `data:` lines
with `"\n"` and returns the `ServerSentEvent`. Lines starting with `:` are
comments — dropped entirely.

The critical edge case: a line split across two reads. If the buffer ends
with partial bytes (no line break), `drainCompleteLines` returns those
trailing bytes unconsumed. They stay in the buffer and are prepended to
the next chunk. At EOF, any remaining bytes are decoded as a final line
and the state is flushed.

### 5. The idle watchdog

All three providers implement the same watchdog pattern. In Anthropic's
`runStream`:

```go
streamCtx, cancel := context.WithCancel(parent)
activity := make(chan struct{}, 1)
done := make(chan struct{})
var idleFired atomic.Bool
go watchIdle(done, activity, cancel, &idleFired)
defer close(done)
```

`watchIdle` at `stream.go:292-315` starts a timer for `streamIdleTimeout`
(90s by default). On each SSE event, the callback sends on `activity` (a
buffered-1 channel — non-blocking, so a backlogged watchdog does not stall
the reader). The watchdog resets its timer on activity. If the timer fires,
it sets `idleFired = true` and calls `cancel()`.

After SSE iteration ends, `streamError` checks `idleFired.Load()`: if true,
the error is wrapped as `StreamTruncated` rather than treated as a real
transport failure. This distinction matters for the agent loop's retry
decision — a truncated stream from a hung connection might be retryable,
while a genuine HTTP error might not.

### 6. Role routing

`ResolveRoles` at `adapter/roles.go:31-49` takes a `RoleRoutingConfig` with
three model specs (`DefaultModel`, `PlanModel`, `SmolModel`) and builds
three `ProviderRouter` instances. Empty plan and smol models inherit the
default. Credentials are resolved through a `SecretLookup` function —
typically a closure over a secrets store — rather than from environment
variables directly, so role-based routing works with credential managers.

### 7. Faux provider replay

When `faux.Stream` is called:

1. Dequeue the next `ResponseStep` (error if queue is empty).
2. Call the step function synchronously to get the final
   `types.AssistantMessage`.
3. Launch a goroutine (`produce`) that:
   - Pushes `EvStart` with the output message as `Partial`.
   - For each content block, pushes start/delta/end events. Text blocks
     are split into token-sized fragments by `splitStringByTokenSize`
     using a seeded `*rand.Rand`.
   - Tool calls include progressive `EvToolCallDelta` events with partial
     argument JSON.
   - Pushes `EvDone` (or `EvError` if the step returned an error).
   - Respects `ctx.Done()` for cancellation/abort.

The replay is deterministic given the same seed and response queue, so
tests can assert exact event sequences.

## Failure modes and invariants

### Unknown provider → config error at startup

`ValidateModelSpec` rejects providers that are not `"anthropic"` or
`"openai"` (the empty provider defaults to Anthropic in `resolveRoute`, not
in validation). The error is returned during `NewProviderRouter`, before any
HTTP machinery exists. Misconfigurations fail loudly at the config boundary.

`ProviderRouter` also checks that the resolved API id has a registered
stream function. If `registry.Resolve(rt.API)` returns `ok == false` or
`StreamSimple == nil`, that is also a startup error. This catches the case
where a provider is valid in `resolveRoute` but the registry is missing its
implementation.

### Missing credentials → error event on first stream call

`assertRequestAuth` in each provider checks for an API key or authorization
header. If both are missing, it returns an error that becomes an `EvError`
event. The stream is never opened; no HTTP request is made. This is a
runtime error (credentials might be loaded lazily), not a config error, so
it arrives through the stream.

For Anthropic specifically, the precedence is `ANTHROPIC_OAUTH_TOKEN` then
`ANTHROPIC_API_KEY`. If both are set, `OAUTH_TOKEN` wins. An explicit bearer
token (`ANTHROPIC_AUTH_TOKEN`) is injected as an `Authorization: Bearer`
header only when no API key is set and no existing authorization header is
present.

### Unterminated stream → error

`iterateAnthropicEvents` tracks `sawMessageStart` and `sawMessageEnd`. If
the stream delivers `message_start` but never `message_stop` (the connection
closed or the SSE stream ended prematurely), the function returns an error:
`"Anthropic stream ended before message_stop"`. This is distinct from the
idle watchdog — the watchdog fires when no bytes arrive; the bracketing
check fires when bytes arrived but the protocol sequence is incomplete.

OpenAI has a parallel check: if the stream ends without any chunk carrying
a `finish_reason`, the response is treated as truncated
(`"stream ended without finish_reason"` at `openai/stream.go:168-170`).

### Idle watchdog → StreamTruncated, not a transport error

The watchdog goroutine sets `idleFired = true` before calling `cancel()`,
and `streamError` checks `idleFired.Load()` after SSE iteration. If the
watchdog fired, the error is wrapped:

```go
// anthropic/stream.go:275-283
func streamError(parentErr error, err error, idle bool) error {
    if parentErr != nil {
        return parentErr  // real cancellation: user typed /cancel
    }
    if idle {
        return StreamTruncated(errors.New("response stream was truncated"))
    }
    return err
}
```

This lets the agent loop distinguish:
- Parent context cancelled → user abort (not an error).
- Watchdog fired → stream hung (StreamTruncated, potentially retryable).
- Real transport error → network/protocol failure (not retryable by default).

### Panic recovery

Every `runStream` has a deferred `recover()` that pushes the panic value
as an error event on the stream. A provider bug that panics does not take
down the agent loop; it surfaces as a terminal `EvError` on that stream.

### Non-mutating credential injection

`WithEnvAPIKey` returns a shallow copy when it fills a key from the
environment; it returns the original pointer when the key is already set or
the env is empty. Callers that hold the original `StreamOptions` cannot
observe the mutation. The test at `base/provider_test.go:56-66` asserts
this explicitly.

### Maximum request size

Anthropic's `runStream` checks the marshaled body size against
`maxAnthropicRequestBytes` before sending. An oversized request becomes an
error event without an HTTP call. This prevents sending a request that the
API would reject mid-stream after consuming the entire body.

## TypeScript to Go

### SSE parsing: `fetch` + `ReadableStream` vs `bufio` + scanner

In the TypeScript reference, an SSE stream is consumed through the Fetch
API's `ReadableStream`:

```typescript
const response = await fetch(url, { body, headers });
const reader = response.body.getReader();
const decoder = new TextDecoder();
let buffer = "";
while (true) {
  const { done, value } = await reader.read();
  if (done) break;
  buffer += decoder.decode(value, { stream: true });
  // split buffer on \n\n, process complete events
}
```

This works because the JS event loop is single-threaded: `await reader.read()`
yields the event loop, and the SSE processing happens between reads without
any concurrent mutation of `buffer`.

Go has no event loop. The equivalent pattern — reading in a goroutine and
sending parsed events on a channel — requires explicit buffer ownership.
`sseparse.IterateSSE` owns the buffer entirely: it reads into a 4096-byte
chunk, appends to an internal `[]byte`, extracts complete lines, and calls
the callback synchronously before the next read. The callback (e.g.,
`foldAnthropicEvent`) runs on the same goroutine as the read loop. There is
no concurrent access to the buffer; no mutex needed.

The TypeScript version often does a `buffer.split("\n\n")` to find event
boundaries. Go's `IterateSSE` works line-by-line instead: each line is
decoded into fields accumulated in `decoderState`, and a blank line
triggers `flushSseEvent`. This is less allocation-heavy than splitting on
double-newlines and joining fields from the split fragments.

### Provider selection: dynamic dispatch vs `Registry`

TypeScript can build a provider at runtime by name:

```typescript
const provider = providerRegistry[name];
if (!provider) throw new Error(`Unknown provider: ${name}`);
return provider.stream(model, context, opts);
```

This works because JS objects are dynamic maps and functions are
first-class. In Go, a `map[string]Provider` achieves the same thing, but
the Provider is a concrete struct with function fields, not an interface.
The Go version adds compile-time guarantees: `Provider.StreamSimple` is a
typed function, not an arbitrary callable. The resolver checks
`p.StreamSimple == nil` at construction time, so a registered provider
without a stream function is caught before any request is made.

### Error handling: `try/catch` vs stream error events

TypeScript providers throw on auth failures, network errors, and parse
errors. The caller wraps the stream call in `try/catch`:

```typescript
try {
  for await (const event of provider.stream(model, ctx, opts)) {
    yield event;
  }
} catch (err) {
  yield { type: "error", error: err };
}
```

Go providers push errors as `EvError` stream events. The stream itself
never returns an error from `Stream()` — it always returns a valid
`*stream.AssistantStream`. This is a deliberate inversion: the caller does
not need two error paths (return value + stream event). All errors — auth,
network, parse, idle timeout, protocol bracketing — arrive through the
same channel as `EvError`. The `pushError` helper in each provider sets
the `StopReason` to either `StopAborted` (parent context done) or
`StopError` (genuine failure), and the agent loop switches on that.

### Interface-based injection: `agent.StreamFn`

The agent loop does not know about providers. It consumes `agent.StreamFn`:

```go
type StreamFn func(ctx context.Context, model types.Model,
    c types.Context, opts *types.SimpleStreamOptions) *stream.AssistantStream
```

This is a consumer-defined function type, not an interface. The adapter
package wires `base.SimpleStreamFunc` into `agent.StreamFn` through
`withStream`, which injects credentials and optionally wraps retry. The
agent loop gets one callable; it does not choose a provider, inspect
headers, or classify errors. That separation means the agent loop compiles
and tests against the `faux` provider without importing any provider
package.

## Where it lives

| File | Key symbols |
|---|---|
| `internal/provider/base/provider.go` | `StreamFunc`, `SimpleStreamFunc`, `Provider`, `Registry`, `NewRegistry`, `Register`, `Resolve`, `WithEnvAPIKey`, `envKeysByProvider`, `getEnvAPIKey`, `deriveEnvVar` |
| `internal/provider/base/provider_test.go` | `TestRegistryResolve`, `TestWithEnvAPIKey`, `TestWithEnvAPIKeyDerivedVar`, `TestStreamFuncSeam` |
| `internal/adapter/modelspec.go` | `modelSpec`, `parseModelSpec`, `ModelIDFromSpec`, `ValidateModelSpec`, `ValidateBaseURL`, `route`, `resolveRoute` |
| `internal/adapter/modelspec_test.go` | `TestParseModelSpec`, `TestModelIDFromSpec`, `TestResolveRouteDefaultsToAnthropic`, `TestResolveRouteUnknownProviderErrors` |
| `internal/adapter/provider.go` | `AnthropicMessagesAPI`, `OpenAICompletionsAPI`, `GoogleGenerativeAPI`, `RoutingConfig`, `ProviderRouter`, `NewProviderRouter`, `withStream`, `erroredStream`, `ResolveProviderForModel` |
| `internal/adapter/provider_test.go` | `TestProviderRouterUnsupportedProviderErrors`, `TestProviderRouterOpenAIRouteUsesOwnCredentials`, `TestProviderRouterBaseURLAndAuthTokenAreHonored`, `TestProviderRouterRetriesRateLimitedResponse` |
| `internal/adapter/roles.go` | `RoleRoutingConfig`, `RoleRouters`, `ResolveRoles`, `roleCredentials` |
| `internal/transport/sseparse/sse.go` | `ServerSentEvent`, `IterateSSE`, `decodeSseLine`, `flushSseEvent`, `drainCompleteLines`, `decoderState` |
| `internal/transport/sseparse/sse_test.go` | SSE round-trip and edge-case tests |
| `internal/provider/anthropic/stream.go` | `Stream`, `StreamSimple`, `runStream`, `watchIdle`, `streamError`, `pushError` |
| `internal/provider/anthropic/events.go` | `rawAnthropicEvent`, `foldState`, `foldBlock`, `iterateAnthropicEvents`, `foldAnthropicEvent`, `foldContentBlockStart`, `foldContentBlockDelta`, `foldContentBlockStop` |
| `internal/provider/anthropic/options.go` | `Options`, `Effort`, `StreamIdleTimeoutFromEnv`, `buildBaseOptions`, `mapThinkingLevelToEffort` |
| `internal/provider/openai/stream.go` | `Stream`, `StreamSimple`, `runStream`, `watchIdle` |
| `internal/provider/openai/events.go` | `openAIStreamChunk`, `openAIFoldState`, `iterateOpenAIEvents`, `foldOpenAIChunk`, `foldOpenAIToolCallDelta` |
| `internal/provider/openai/options.go` | `Options`, `StreamIdleTimeoutFromEnv` |
| `internal/provider/google/stream.go` | `Stream`, `StreamSimple`, `runStream`, `watchIdle` |
| `internal/provider/google/events.go` | `googleStreamChunk`, `googleFoldState`, `iterateGoogleEvents`, `foldGoogleChunk`, `foldGoogleFuncCall` |
| `internal/provider/google/options.go` | `Options`, `thinkingConfig`, `StreamIdleTimeoutFromEnv` |
| `internal/provider/faux/faux.go` | `Faux`, `ResponseStep`, `State`, `New`, `SetResponses`, `AppendResponses`, `Stream`, `StreamSimple`, `Provider`, `Respond`, `RespondMessage`, `Text`, `Thinking`, `ToolCall` |
| `internal/engine/types/options.go` | `ProviderResponse`, `StreamOptions`, `SimpleStreamOptions` (with `OnResponse`, `OnPayload`) |
| `internal/engine/types/model.go` | `Model` (with `API`, `Provider`, `BaseURL`, `ContextWindow`, `MaxTokens`, `Compat`) |
| `internal/engine/types/stream.go` | `StreamEvent`, `StreamEventType`, `AssistantStream`, `AssistantBuilder` |
