# 12: Idiomatic Go versus TypeScript

## The problem

We ported a TypeScript coding agent to Go. A direct line-by-line translation
produces subtle, hard-to-debug defects because the two languages disagree on
nearly every dimension that matters for a concurrent, cancellation-aware,
tool-calling runtime:

| Dimension | TypeScript | Go |
|---|---|---|
| Concurrency | Single-threaded event loop | Goroutines + channels |
| Type system | Structural, `unknown`/`any` loose | Nominal, `any` is a code smell |
| Interfaces | Implicit, duck-typed | Explicit, consumer-defined |
| Errors | `throw`/`catch`, `instanceof`, string-match | Returned `error` values, `Is`/`As`/`Unwrap` |
| Cancellation | `AbortSignal` threaded through | `context.Context` as first parameter |
| State | Shared mutable objects, no races | Explicit ownership, `sync.Mutex`, `-race` |
| Dependencies | Mock `setTimeout`/`Date.now` | Inject `Clock`/`Sleeper`/`Random` interfaces |
| Filesystem | No sandbox; path traversal by convention | `os.Root` containment |
| Assets | `fs.readFileSync` at runtime | `go:embed` at compile time |
| Math | JS floats, no overflow | Integer overflow is silent in Go; bounds-check explicitly |

This module is the capstone: it collects every recurring idiomatic-Go pattern the
harness uses and shows what the TypeScript reference did, what we do instead, and
why the language forces or rewards the difference. It references the other
modules by number; read those first for the subsystem details.

## Key decisions and the thinking process

This module is the design contract: every subsystem that touches the public
boundary must follow these rules. The decisions below are the ones that came up
in every review, every package, and every race-condition postmortem.

### Consumer-defined interfaces, not implementer-defined ones

In TypeScript, an object satisfies an interface by shape. The interface often
lives next to the implementation — a class `implements` it, or a module exports
both the interface and the implementation. In Go, the interface belongs to the
consumer. You define the narrowest interface you actually need and declare it
right above the function that accepts it.

This harness defines **every** interface in the package that calls it:

- `retry.Streamer` (`internal/retry/stream.go:16-18`) — the retry decorator
  needs to call `Stream`. The provider packages don't know this interface exists.
- `retry.Clock` / `retry.Sleeper` / `retry.Random` (`internal/retry/config.go:21-33`) —
  the retry package needs time, sleep, and randomness. It declares those needs
  as three single-method interfaces.
- `config.SecretResolver` (`internal/config/config.go:102-104`) — the config
  package needs to fetch credentials. It declares the interface. `cmd/harness`
  supplies a concrete resolver.
- `tools.ReadResourceResolver` (`internal/tools/tools.go:108-110`) — the read
  tool needs to resolve `skill://` and internal URIs. It declares the interface;
  the skills package implements it.

The TypeScript agent had provider classes with `stream()` methods and a shared
abstraction in a base class. We rejected that: a Go interface with one method is
cheaper, testable with a function literal, and never forces implementers into an
inheritance hierarchy.

### Accept interfaces, return structs

Every public constructor follows this rule:

```go
// Accepts the Streamer interface, returns the concrete *Retrier struct.
func New(next Streamer, cfg Config) (*Retrier, error)
```

`internal/retry/stream.go:35-43` validates both the interface value (must be
non-nil) and the configuration, then returns a concrete `*Retrier`. The caller
can't see the fields; they get a stable API. If the retry package later adds
internal state, no caller breaks.

The TypeScript approach was to return an object conforming to a shared
`StreamProvider` type. That meant every caller could depend on any field or
method the object happened to have. Go's unexported struct fields close that
door: `Retrier` has only private fields (`internal/retry/stream.go:29-32`).

### context.Context everywhere, never stored

Every potentially blocking public operation takes `context.Context` as its first
parameter. No function caches a request context in a struct field.

Examples from across the codebase:

- `Streamer.Stream(ctx context.Context, ...)` — `internal/retry/stream.go:17`
- `Sleeper.Sleep(ctx context.Context, delay time.Duration) error` — `internal/retry/config.go:27`
- `Resolver.Resolve(ctx context.Context, rawURI string)` — `internal/skills/resolver.go:84`
- `Stream.Result(ctx context.Context) (R, error)` — `internal/engine/stream/stream.go:132`

Cancellation flows down through the entire call tree. The TypeScript reference
used `AbortSignal` which could be passed around or stored; Go's `context` is
always threaded explicitly. A goroutine that ignores its context leaks.

### Typed errors: stable codes, `Unwrap`/`Is`/`As`, never string-match

The TypeScript agent threw `Error` subclasses and caught with `instanceof`. That
works in a single-threaded runtime but breaks under wrapping (a retry decorator
wrapping a provider error). Go's solution is explicit wrapping and sentinel
matching.

The retry package defines stable error codes as typed constants:

```go
type Code string

const (
    CodeRateLimited     Code = "rate_limited"
    CodeServerFailure   Code = "server_failure"
    CodeNetworkFailure  Code = "network_failure"
    CodeAttemptTimeout  Code = "attempt_timeout"
    CodeStreamInterrupted Code = "stream_interrupted"
    CodeContextOverflow Code = "context_overflow"
    CodeCanceled        Code = "canceled"
    // ...
)
```

`internal/retry/error.go:15-28`

Every error type implements three methods:

1. `Error() string` — human-readable, stable text
2. `Unwrap() error` — returns the underlying cause for `errors.Is`/`errors.As` traversal
3. `Is(target error) bool` — matches sentinels AND typed error codes

`internal/retry/error.go:52-87` for `retry.Error`; `internal/config/config.go:205-208` for `config.Error`;
`internal/tools/tools.go:119-128` for `tools.ReadResourceError`.

Sentinels let callers check categories without importing the error package's
internals:

```go
var (
    ErrRetryable = errors.New("retryable provider failure")
    ErrTerminal  = errors.New("terminal provider failure")
    ErrExhausted = errors.New("retry attempts exhausted")
    ErrCanceled  = errors.New("retry canceled")
)
```

`internal/retry/error.go:30-39`

A caller checks `errors.Is(err, retry.ErrRetryable)` — they don't need to know
the error type, the code mapping, or the wrapping depth. String matching is a
compatibility fallback inside `Classify` (`internal/retry/error.go:100`), never
the public API.

### No public `any` or `map[string]any`

The TypeScript agent freely used `unknown`, `any`, and `Record<string, unknown>`
at every boundary. The Go harness bans both from the public API. Every value is
typed.

`ResolvedConfig` (`internal/config/config.go:121-140`) has exclusively private
fields with typed accessors. The system prompt is rendered from `PromptData`
(`internal/prompt/system.go:30-62`), a struct with boolean flags, string fields,
and typed slices — zero `any` fields.

The one extension point is the tool argument/result boundary. Tools accept
arbitrary JSON from the model. The harness uses `json.RawMessage` at exactly
that boundary:

```go
type AgentTool struct {
    types.Tool
    Label            string
    PrepareArguments func(args json.RawMessage) json.RawMessage
    Execute          func(ctx context.Context, toolCallID string,
        params json.RawMessage,
        onUpdate AgentToolUpdateCallback) (AgentToolResult, error)
}
```

`internal/agent/types.go:54-57` (signature); every tool implementation receives
`json.RawMessage` and immediately decodes it into a typed private struct
(e.g., `internal/tools/tools.go:485-487` for the read tool). The raw JSON is
never threaded deeper.

### Dependency injection: Clock, Sleeper, Random

TypeScript tests mock `setTimeout` and `Date.now` with module-level overrides.
That's global mutable state — it breaks under concurrent tests. Go makes every
side-effectful dependency an explicit field:

```go
type Config struct {
    MaxAttempts int
    BaseDelay   time.Duration
    BackoffCap  time.Duration
    MaxDelay    time.Duration
    Jitter      Jitter
    Clock       Clock
    Sleeper     Sleeper
    Random      Random
}
```

`internal/retry/config.go:36-45`

`Clock` supplies `Now()`, `Sleeper` sleeps with cancellation, and `Random`
returns `[0,1)` samples. Production uses `realClock{}`, `timerSleeper{}`, and a
mutex-protected `lockedRandom` (`internal/retry/config.go:47-81`).

Tests inject fake implementations: a fixed clock, a no-op sleeper that records
delays, and a deterministic random sequence. No module mocking, no global state
reset between tests.

The config package applies the same pattern: `Config.LookupEnv` is an injected
`func(string) string` (`internal/config/config.go:116`). Production passes
`os.Getenv`; tests pass a map lookup. This prevents `os.Getenv` reads from
leaking into tests (an adversarial review caught exactly this bug).

### Defensive copies and the `-race` detector

Go's race detector (`go test -race`) catches concurrent reads and writes on
shared memory. TypeScript's single-threaded event loop never has this problem,
so the reference code freely returned references to mutable internal state.

The harness copies at every ownership boundary:

- `ResolvedConfig.SkillPaths()` returns `append([]string(nil), c.skillPaths...)`
  (`internal/config/config.go:191-193`) — the caller gets a copy, not the
  internal slice.
- `AssistantStream.Final()` deep-copies the message before returning it
  (`internal/engine/stream/assistant.go:62-68`) — the caller can't mutate the
  stream's internal state.
- The retry decorator copies every forwarded event through `copyEvent`,
  `copyResponse`, and `cloneMessage` (`internal/retry/stream.go:242-277`) —
  retried attempts don't alias with downstream consumers.

Module 07 documents the live-`Partial` race we shipped and caught under `-race`:
the stream returned a pointer to internal mutable state, and a downstream
goroutine read it while the producer wrote to it. The fix was a defensive copy
at the handoff point.

### `os.Root` filesystem containment

TypeScript has no filesystem sandbox. Path traversal is prevented by convention
and `path.resolve` checks. The Go harness uses `os.Root` (Go 1.24+) for
skill resolution:

```go
root, err := os.OpenRoot(skill.BaseDir)
if err != nil {
    return resource.Content{}, &ResolveError{...}
}
defer root.Close()
info, err := root.Stat(target)
```

`internal/skills/resolver.go:120-124`

`os.Root` confines all filesystem operations to `skill.BaseDir`. Even if the
target path contains `..` segments, `OpenRoot` resolves them within the base.
The resolver adds an explicit `..`-component check first
(`internal/skills/resolver.go:114-118`) and a symlink escape check
(`internal/skills/resolver.go:180-198`) as defense in depth.

### `go:embed` for templates and assets

TypeScript reads template files at runtime with `fs.readFileSync`. If the file
is missing, the error surfaces in production. Go embeds assets at compile time:

```go
//go:embed templates/system.md.tmpl
var systemTemplateText string

var systemTemplate = template.Must(template.New("system.md.tmpl").Funcs(...).Parse(systemTemplateText))
```

`internal/prompt/system.go:64-77`

`template.Must` panics at init if the template is malformed — the binary won't
start. The template is a static `string`, not a file path; it cannot be missing
at runtime. The tradeoff: changing the template requires a rebuild. The payoff:
the binary is self-contained, and template errors are compile-time failures,
not runtime surprises.

### Saturating and ceiling arithmetic

TypeScript numbers are IEEE 754 doubles; integer overflow rounds to
`Number.MAX_SAFE_INTEGER` or `Infinity`. Go integers wrap silently. The harness
bounds-checks every arithmetic conversion from user-controlled values.

The config package guards millisecond-to-Duration conversion:

```go
// maxRetryDurationMS is the largest millisecond value convertible to a
// time.Duration (int64 nanoseconds) without overflow.
const maxRetryDurationMS = math.MaxInt64 / 1_000_000
```

`internal/config/config.go:236-238`

The retry hints package applies ceiling with overflow rejection:

```go
func durationCeilMillis(value float64, unit time.Duration) (time.Duration, bool) {
    millis := value * (float64(unit) / float64(time.Millisecond))
    if math.IsNaN(millis) || millis <= 0 {
        return 0, false
    }
    millis = math.Ceil(millis)
    if millis >= float64(math.MaxInt64)/float64(time.Millisecond) {
        return 0, false
    }
    return time.Duration(millis) * time.Millisecond, true
}
```

`internal/retry/hints.go:86-95`

The jitter delay function saturates at `MaxInt64` rather than wrapping:

```go
if delay >= float64(math.MaxInt64) {
    return time.Duration(math.MaxInt64)
}
```

`internal/retry/config.go:160-161`

Every user-supplied or provider-supplied numeric value that becomes a
`time.Duration` passes through an overflow guard. An adversarial review caught
several edge cases where these guards were missing or off-by-one.

### No global mutable state

TypeScript modules often export mutable singletons — a default client, a shared
config object, a module-level counter. Go bans this. The harness has no global
mutable registries, no package-level `var` that changes after initialization,
and no `init()` functions that read the environment.

The retry package's `DefaultConfig()` creates a **new** `lockedRandom` with a
**new** seed on every call (`internal/retry/config.go:83-96`). Two callers get
independent RNGs.

The mutation queue for file operations is initialized once at package init and
never reassigned (`internal/tools/tools.go:178`) — it's a constant singleton,
not mutable global state.

Provider registries are constructed per router during startup, never stored in
package-level variables. The config is resolved once into an immutable
`ResolvedConfig` and passed down by value or pointer to read-only fields.

## Signatures and types

A few emblematic signatures that capture the patterns above:

```go
// Consumer-defined interface — the retry package needs only Stream.
type Streamer interface {
    Stream(context.Context, types.Model, types.Context, *types.SimpleStreamOptions) *stream.AssistantStream
}

// Dependency injection — explicit Clock, Sleeper, Random fields.
type Config struct {
    MaxAttempts int
    BaseDelay   time.Duration
    BackoffCap  time.Duration
    MaxDelay    time.Duration
    Jitter      Jitter
    Clock       Clock
    Sleeper     Sleeper
    Random      Random
}

// Typed error with stable Code, Unwrap, and Is.
type Error struct {
    Code       Code
    Message    string
    Retryable  bool
    RetryAfter time.Duration
    Attempts   int
    Cause      error
}
func (e *Error) Error() string
func (e *Error) Unwrap() error
func (e *Error) Is(target error) bool

// Immutable resolved config — all fields private, typed accessors only.
type ResolvedConfig struct {
    model           string
    maxTokens       int
    enableWeb       bool
    skillPaths      []string
    // ...all private
}
func (c *ResolvedConfig) Model() string
func (c *ResolvedConfig) MaxTokens() int
func (c *ResolvedConfig) SkillPaths() []string  // returns defensive copy

// json.RawMessage at the one extension boundary.
type AgentTool struct {
    types.Tool
    Label            string
    PrepareArguments func(args json.RawMessage) json.RawMessage
    Execute          func(ctx context.Context, toolCallID string,
        params json.RawMessage,
        onUpdate AgentToolUpdateCallback) (AgentToolResult, error)
}

// os.Root-confined file read.
func (r *Resolver) Resolve(ctx context.Context, rawURI string) (resource.Content, error)
```

## How it works

The patterns compose into a layered safety perimeter. Here is the data flow from
a provider call through retry to the session, annotated with the relevant
idiomatic-Go pattern at each hop:

1. **Provider call** (`Streamer.Stream`) — the retry package calls the narrow
   interface it defined. `context.Context` flows in as the first parameter.
2. **Retry decorator** (`Retrier.produce`) — copies every event before
   forwarding (`retry/stream.go:242-277`). On failure, `Classify` returns a
   typed `*Error` with a stable `Code`. The decorator checks `errors.Is`
   against `ErrRetryable`/`ErrTerminal` — never string-matches.
3. **Backoff** (`Sleeper.Sleep`) — the injected `Sleeper` sleeps while honoring
   the caller's context. A test can inject a fake that records delays without
   blocking. `Clock.Now()` supplies the time; `Random.Float64()` supplies
   independent jitter samples.
4. **Skill resolution** (`Resolver.Resolve`) — opens a skill directory through
   `os.Root`, reads the file, returns `resource.Content`. The caller cannot
   escape the base directory.
5. **System prompt** (`BuildSystemPrompt`) — renders the `go:embed`-ded template
   from a typed `PromptData` struct. If a tool name is missing from the registry,
   the template never mentions it (module 09).
6. **Config resolution** (`Config.Resolve`) — reads flags, env, settings once.
   Validates all numeric bounds against `math.MaxInt64` overflow guards. Returns
   an immutable `*ResolvedConfig` with private fields and typed accessors. No
   secret data survives resolution.
7. **Stream result** (`Stream.Result`) — blocks with a `context.Context`. Returns
   the terminal result or `ErrNoResult`. The stream channel is
   producer-owned (module 07).

## Failure modes and invariants

### The Partial race

The most instructive bug we shipped: a stream event carried a `Partial` pointer
into the assistant message. The producer goroutine continued writing to the
underlying struct while a downstream goroutine read it. `-race` caught it; the
fix was a defensive copy at the handoff boundary (see module 07).

### Overflow in duration conversion

An adversarial review found that `durationFromFloat` rejected `> MaxInt64` but
allowed `== MaxInt64`. Because
`float64(math.MaxInt64)` rounds to `2^63`, the equality case could produce a
negative `time.Duration`. The fix: use `>=` and bounds-check in source-unit
space before multiplication.

### Environment bypass in config resolution

An adversarial review found that `Resolve` called an `os.Getenv`-reading helper
despite accepting an injected `LookupEnv`.
The injected environment is the contract; any direct `os.Getenv` read below
the resolver violates the DI boundary and makes tests non-hermetic. Fix:
use the injected lookup exclusively.

### Error wrapping leaking retryability

If a retry decorator wraps a retryable `*Error` as the cause of an exhaustion
error, `errors.Is(exhausted, ErrRetryable)` becomes true — a terminal exhaustion
looks retryable. The fix (`internal/retry/stream.go:149-151`) unwraps the
classified error's `Cause`, not the classified error itself, so the exhaustion
chain terminates cleanly.

### Invariants

- Every exported interface is defined in exactly one consuming package.
- Public constructors return concrete struct pointers, never interfaces.
- Every `time.Duration` derived from user input passes an overflow guard.
- No function stores a `context.Context` in a struct field.
- No public type uses `any` or `map[string]any`.
- Every slice or map returned from an accessor is a copy, not a reference.
- Package-level `var` declarations are either constants or initialized once and
  never mutated.
- `go test -race` passes for every concurrent subsystem.

## TypeScript to Go

This section is the capstone: every recurring idiomatic-Go pattern, what the
TypeScript reference did, what the harness does, and why the language forces
or rewards the difference.

### 1. Consumer-defined interfaces

**TypeScript way:** Define an interface next to or inside the implementing class.
Export both. Callers depend on the concrete class or a shared abstract class.

```typescript
// Defined where implemented
class AnthropicProvider implements StreamProvider {
  async *stream(model: string, messages: Message[]): AsyncGenerator<Event> { ... }
}
```

**Go way:** Define the narrowest interface in the package that needs it.
Implementers never know the interface exists.

```go
// Defined in internal/retry/stream.go — the retry package owns this.
type Streamer interface {
    Stream(context.Context, types.Model, types.Context, *types.SimpleStreamOptions) *stream.AssistantStream
}
```

`internal/retry/stream.go:16-18`

**Why:** A Go interface with one method costs nothing — no virtual dispatch
table, no inheritance tax. When the consumer defines the interface, the
implementer has zero coupling to the consumer's needs. You can test the retry
decorator with a `StreamFunc` adapter (`internal/retry/stream.go:20-26`)
instead of mocking an entire provider.

### 2. Accept interfaces, return structs

**TypeScript way:** A factory function returns an object typed as the shared
interface. Callers only see the interface.

```typescript
function createRetrier(provider: StreamProvider, config: RetryConfig): StreamProvider { ... }
```

**Go way:** Accept the interface, return a concrete `*Retrier`.

```go
func New(next Streamer, cfg Config) (*Retrier, error)
```

`internal/retry/stream.go:35`

**Why:** Returning a concrete struct pointer lets Go inline method calls and
keeps internal fields genuinely private. Returning an interface would force
all callers through the interface dispatch and prevent future methods from
being added without breaking the interface contract. The rule is "accept
interfaces, return structs" — the consumer gets a stable concrete API, and
the implementer can evolve internally.

### 3. context.Context as first parameter

**TypeScript way:** An `AbortSignal` passed through an options object.

```typescript
const stream = provider.stream(model, messages, { signal: abortController.signal });
```

**Go way:** `context.Context` as the first parameter on every blocking call.

```go
func (f StreamFunc) Stream(ctx context.Context, model types.Model, c types.Context, opts *types.SimpleStreamOptions) *stream.AssistantStream
```

`internal/retry/stream.go:24`

**Why:** Go's context carries deadlines, cancellation, and request-scoped values
in one value. Making it the first parameter is a universal convention — any Go
developer knows where to find it. The critical rule is never to store a context
in a struct: contexts are request-scoped, and a stored context outlives its
request. The retrier stores only `next` and `cfg` (`internal/retry/stream.go:29-32`),
never a context.

The harness derives child contexts for provider calls, tool execution, and
subagents. Cancellation propagates through every goroutine. A goroutine that
ignores `ctx.Done()` leaks.

### 4. Typed errors: Unwrap, Is, As, %w

**TypeScript way:** Throw `Error` subclasses, catch with `instanceof`, or
string-match on `.message`.

```typescript
try {
    await provider.stream(model, messages);
} catch (err) {
    if (err instanceof RateLimitError) { ... }
    else if (err.message.includes("context length")) { ... }
}
```

**Go way:** Return `error` values. Define typed errors with `Unwrap()`, `Is()`,
and stable `Code` fields. Use `errors.Is` and `errors.As` at call sites. Never
string-match.

```go
type Error struct {
    Code       Code
    Message    string
    Retryable  bool
    RetryAfter time.Duration
    Attempts   int
    Cause      error
}

func (e *Error) Unwrap() error { return e.Cause }

func (e *Error) Is(target error) bool {
    if target == ErrRetryable && e.Retryable { return true }
    if target == ErrTerminal && !e.Retryable { return true }
    // Also matches by Code for typed comparisons
    ...
}
```

`internal/retry/error.go:42-87`

Callers check `errors.Is(err, retry.ErrRetryable)` — they never inspect the
error string or cast to a concrete type. The `%w` verb wraps causes:

```go
return nil, fmt.Errorf("retry attempt %d: %w", attempt, classified)
```

`errors.Is` traverses the chain. `errors.As` extracts typed errors.

**Why:** Go errors are values, not exceptions. There is no stack unwind, no
`catch` block, no `finally`. Wrapping preserves the cause chain for logging
and debugging while keeping the public API stable. A sentinel like
`ErrRetryable` is a stable contract; the underlying error type can change
without breaking callers.

The harness uses string matching only as a compatibility fallback inside
`Classify` (`internal/retry/error.go:100`), never at a public call site.

### 5. No public `any` or `map[string]any`

**TypeScript way:** `unknown` for generic payloads, `Record<string, unknown>`
for dynamic config, `any` for rapid prototyping.

```typescript
interface ToolCall {
    name: string;
    arguments: Record<string, unknown>;  // whatever the model sent
}
```

**Go way:** Every value is typed. The one extension boundary uses `json.RawMessage`.

```go
type ResolvedConfig struct {
    model           string  // private, typed
    maxTokens       int     // private, typed
    skillPaths      []string
    // ...all private fields, no any
}

// Tool arguments stay as raw JSON at the boundary only
type AgentTool struct {
    Execute func(ctx context.Context, toolCallID string,
        params json.RawMessage, ...) (AgentToolResult, error)
}
```

`internal/config/config.go:121-140`; `internal/agent/types.go:54-57`

Every tool implementation immediately decodes `json.RawMessage` into a typed
private struct:

```go
var input readToolInput
if err := decodeParams(params, &input); err != nil {
    return agent.AgentToolResult{}, err
}
```

`internal/tools/tools.go:486-488`

**Why:** `json.RawMessage` says "this is JSON, and I'm deferring decoding." It's
an explicit, intentional boundary — not a generic escape hatch. The result type
(`AgentToolResult`) also carries `json.RawMessage` for details
(`internal/agent/types.go:44-47`), which adapters render as they choose. But
no value typed as `any` or `map[string]any` crosses a public API boundary.

### 6. Dependency injection for determinism

**TypeScript way:** Mock `setTimeout` with `jest.useFakeTimers()`, mock
`Date.now` with `jest.spyOn(Date, 'now')`, mock random with module-level
overrides.

```typescript
beforeEach(() => {
    jest.useFakeTimers();
    jest.spyOn(Date, 'now').mockReturnValue(fixedTime);
});
```

**Go way:** Inject `Clock`, `Sleeper`, and `Random` interfaces as struct fields.

```go
type Config struct {
    Clock   Clock     // interface { Now() time.Time }
    Sleeper Sleeper   // interface { Sleep(ctx context.Context, delay time.Duration) error }
    Random  Random    // interface { Float64() float64 }
    // ...other fields
}
```

`internal/retry/config.go:36-45`

Production: `DefaultConfig()` creates a real clock, timer-based sleeper, and
newly seeded RNG (`internal/retry/config.go:83-96`).

Tests: inject a fixed clock returning a known time, a sleeper that records
delays to a slice, and a random returning a predetermined sequence. No global
state, no module mocking, no test isolation issues.

**Why:** Global mocking breaks under concurrent tests — one test's `spyOn`
leaks into another's execution. Go's explicit DI makes the dependency visible
in the type signature. You can run every test in parallel with `-race` and
never worry about mock collisions.

### 7. Defensive copies and -race

**TypeScript way:** Return references freely. The single-threaded event loop
guarantees no concurrent mutation.

```typescript
getSkillPaths(): string[] {
    return this.skillPaths;  // direct reference, safe in JS
}
```

**Go way:** Copy at every ownership boundary. Run `go test -race` on every
concurrent subsystem.

```go
func (c *ResolvedConfig) SkillPaths() []string {
    return append([]string(nil), c.skillPaths...)
}
```

`internal/config/config.go:191-193`

```go
func (a *AssistantStream) Final() *types.AssistantMessage {
    if a.final != nil {
        msg := *a.final
        return &msg
    }
    msg := a.builder.Message()
    return &msg
}
```

`internal/engine/stream/assistant.go:62-68`

The retry decorator deep-copies every forwarded event, response, and message
(`internal/retry/stream.go:242-277`) because the provider may reuse buffers
across attempts.

**Why:** Go's goroutines share memory. If you return a pointer to internal state
and another goroutine mutates that state, you have a data race. The `-race`
detector catches these at test time, but the fix is always a copy at the
boundary. The rule is: the producer owns the memory, the consumer gets a
snapshot.

### 8. os.Root filesystem containment

**TypeScript way:** Resolve paths with `path.resolve(base, userInput)` and
check for `..` traversal manually. No OS-level sandbox.

```typescript
const resolved = path.resolve(skillDir, userPath);
if (!resolved.startsWith(skillDir)) throw new Error("path traversal");
```

**Go way:** Use `os.Root` (Go 1.24+) plus explicit path validation.

```go
// Validate path components first
for _, part := range strings.FieldsFunc(target, func(r rune) bool { return r == '/' || r == filepath.Separator }) {
    if part == ".." {
        return resource.Content{}, &ResolveError{Code: ResolvePathEscape, ...}
    }
}
// Then use os.Root — the OS enforces containment
root, err := os.OpenRoot(skill.BaseDir)
// ...
info, err := root.Stat(target)
```

`internal/skills/resolver.go:114-126`

**Why:** `os.Root` is an OS-level guarantee (backed by `openat` on Linux).
Even if the Go-level path checks have a bug, the kernel rejects access outside
the root. The explicit `..` check and symlink resolution
(`internal/skills/resolver.go:180-198`) provide defense in depth. The Go
standard library gives you a real sandbox; TypeScript's filesystem is the
process's full filesystem.

### 9. go:embed for templates and assets

**TypeScript way:** Read files at runtime.

```typescript
const template = fs.readFileSync(path.join(__dirname, "system.md.tmpl"), "utf-8");
```

**Go way:** Embed at compile time.

```go
//go:embed templates/system.md.tmpl
var systemTemplateText string

var systemTemplate = template.Must(template.New("system.md.tmpl").Funcs(...).Parse(systemTemplateText))
```

`internal/prompt/system.go:64-77`

**Why:** `go:embed` copies the file into the binary at build time. The binary is
self-contained — no asset directory to deploy, no missing-file runtime errors.
`template.Must` panics at init if the template is invalid, so malformed templates
are a build failure, not a production surprise. The tradeoff is that changing
the template requires a rebuild, but for a shipped binary, that's the right
tradeoff.

### 10. Saturating and ceiling arithmetic

**TypeScript way:** Numbers are IEEE 754 doubles. Integer overflow rounds to
safe integers or infinity. No explicit bounds checking needed for most use
cases.

```typescript
const delayMs = Math.ceil(value * 1000);  // JS float, no overflow concern at practical values
```

**Go way:** Every integer conversion from user input to `time.Duration` passes
an explicit overflow guard.

```go
const maxRetryDurationMS = math.MaxInt64 / 1_000_000  // internal/config/config.go:238

// Ceil to millis with overflow rejection
millis = math.Ceil(millis)
if millis >= float64(math.MaxInt64)/float64(time.Millisecond) {
    return 0, false
}
return time.Duration(millis) * time.Millisecond, true
```

`internal/retry/hints.go:91-95`

**Why:** Go integers wrap on overflow. `time.Duration(math.MaxInt64 + 1)` is
a negative number — a jitter or retry delay that should be a cap becomes a
zero or negative value, silently disabling the safety limit. Every arithmetic
path that converts a user- or provider-supplied value to a `time.Duration`
must check against `math.MaxInt64` in the correct unit space. An adversarial
review found off-by-one errors in these guards;
every one of those would have been a production bug.

### 11. No global mutable state

**TypeScript way:** Module-level singletons are idiomatic.

```typescript
export const defaultClient = new AnthropicClient();
export let activeRequestCount = 0;
```

**Go way:** No package-level `var` that changes after initialization.

```go
// DefaultConfig creates a NEW RNG per call — two callers get independent state.
func DefaultConfig() Config {
    seed := uint64(time.Now().UnixNano())
    return Config{
        Random: &lockedRandom{r: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))},
        ...
    }
}
```

`internal/retry/config.go:83-96`

The only package-level `var` that persists is either `const`, initialized once
and never mutated (the template in `internal/prompt/system.go:65,75`), or a
constant singleton (the mutation queue in `internal/tools/tools.go:178`).

**Why:** Global mutable state is the enemy of concurrent testing and
deterministic behavior. Two tests running in parallel that both call
`DefaultConfig()` get independent RNGs with independent seeds. No test can
accidentally depend on state left by another test. The `-race` detector
verifies this invariant.

## Where it lives


### Source packages (real file:line throughout)

| Pattern | Files |
|---|---|
| Consumer-defined interfaces | `internal/retry/stream.go:16-18`, `internal/retry/config.go:21-33`, `internal/config/config.go:102-104`, `internal/tools/tools.go:108-110` |
| Accept-interface/return-struct | `internal/retry/stream.go:35-43` |
| context.Context first | `internal/retry/stream.go:17,24`, `internal/retry/config.go:27`, `internal/skills/resolver.go:84`, `internal/engine/stream/stream.go:132` |
| Typed errors + Unwrap/Is | `internal/retry/error.go:15-87`, `internal/config/config.go:199-208`, `internal/tools/tools.go:113-128` |
| No public any/map[string]any | `internal/config/config.go:121-140`, `internal/prompt/system.go:30-62` |
| json.RawMessage boundary | `internal/agent/types.go:54-57`, `internal/tools/tools.go:485-487` |
| DI (Clock/Sleeper/Random) | `internal/retry/config.go:20-96` |
| DI (LookupEnv) | `internal/config/config.go:114-118,241-245` |
| Defensive copies | `internal/config/config.go:191-193`, `internal/engine/stream/assistant.go:62-68`, `internal/retry/stream.go:242-277` |
| os.Root containment | `internal/skills/resolver.go:84-178` |
| go:embed | `internal/prompt/system.go:64-77` |
| Overflow guards | `internal/config/config.go:236-238`, `internal/retry/hints.go:86-95`, `internal/retry/config.go:160-161` |
| No global mutable state | `internal/retry/config.go:83-96`, `internal/tools/tools.go:178` |

### Cross-references to other modules

- **Module 07** (Concurrency): the live-`Partial` race, channel ownership,
  `-race` detector discipline
- **Module 04** (Config): the `ResolvedConfig` immutability story, `LookupEnv` DI
- **Module 06** (Retry): the typed error taxonomy, `Classify` precedence, sentinels
- **Module 05** (Skills): `os.Root` containment in detail
- **Module 09** (Prompt): `go:embed` template rendering, typed `PromptData`
- **Module 03** (Tools): `json.RawMessage` at the tool boundary
