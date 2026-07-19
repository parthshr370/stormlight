# 10 -- Permissions and approval

## The problem

An agent with a bash tool and a write tool can do anything the user can do: delete
files, install packages, push commits, run `rm -rf`. Without a gate, the harness
is a loaded footgun handed to a language model that cannot distinguish a
deliberate destructive command from a hallucinated one.

The reference harness had four permission modes plus a destructive-command guard
backed by regex. Phase 3b replaced the mode-only system with a tier-based engine
(read/write/exec), added per-tool allow/deny/prompt policy, and fixed the
longstanding `args *any` wart that violated the idiomatic-Go rules in module 12
(idiomatic Go vs TypeScript). This module documents the
shipped design.

Three problems a naive implementation creates and this one avoids:

1. **Scattered checks per tool.** If every tool calls its own `checkPermission`
   helper, the gate can diverge -- the write tool softens its check, the bash tool
   tightens its own, and there is no single place to audit.
2. **Silent bypass from a nil/missing mode.** If the mode is parsed loosely
   (`strings.Contains(mode, "plan")`), a typo like `plann` silently becomes
   bypass. Every mode value must be enumerated.
3. **No plan collection.** Plan mode without a collector is just a mute button:
   the model generates a plan, the user cannot see it, and the harness has no way
   to check progress. Plan mode must *extract and display* the plan, not just
   block mutations.

## Key decisions and the thinking process

### Decision 1: A single chokepoint gate, not per-tool checks

The gate lives at one call site: `BuildAgentStack` registers a `beforeToolCall`
hook at `internal/session/stack.go:95-105`. Every tool invocation -- bash, write,
edit, read, task, anything -- passes through `permission.Gate` before execution.
There is no per-tool permission logic and no way to register a tool that
circumvents the hook.

```go
// stack.go:95-105
beforeToolCall := func(ctx context.Context, c agent.BeforeToolCallContext) *agent.BeforeToolCallResult {
    rawArgs, err := json.Marshal(c.Args)
    if err != nil {
        rawArgs = nil
    }
    allow, reason := permission.Gate(ctx, modeFunc(), c.ToolCall.Name, rawArgs, cfg.PermissionPolicy)
    if allow {
        return nil
    }
    return &agent.BeforeToolCallResult{Block: true, Reason: reason}
}
```

This is wired into the agent via `AgentOptions.BeforeToolCall` at
`stack.go:169`. The agent loop calls this hook after argument validation
(`internal/agent/executor.go:293`) and before dispatching the tool. A blocked
tool returns its reason as the tool result text; the model sees "Plan mode: edit
is disabled because it can mutate the workspace" and can adjust its approach.

The alternative -- per-tool `Approve` callbacks registered alongside each tool's
`Run` function -- was rejected because it splits policy across an unbounded number
of files. A single gate means one function to read, one place to debug, and one
point where new tiers or policy rules land.

### Decision 2: Tiers replace the hardcoded mutating-tool list

The old `isMutatingTool` function (`permission.go:405-415` in the pre-Phase-3b
code) was a hardcoded switch with vendor tool names that did not belong in the
open-source harness. Phase 3b replaced it with `ClassifyTool` at
`internal/permission/policy.go:58-67`, which maps every tool to one of three tiers:

- `TierRead` -- tools that inspect existing data: `read`, `grep`, `find`, `ls`,
  `web_search`, `web_fetch`, `attachment`
- `TierWrite` -- tools that change workspace content: `write`, `edit`
- `TierExec` -- everything else: `bash`, `task`, and any unknown tool name (the
  safe default)

The tier is used by `Gate` at two points: in plan mode, `TierRead` tools are
always allowed and `TierWrite`/`TierExec` tools are blocked (with safe-bash as a
carve-out); and when a `DecidePrompt` override fires, the tier is attached to the
`PromptRequest` so a future interactive Prompter can display the tool's risk
level.

The trade-off: `ClassifyTool` is a central name-to-tier map. Adding a new tool
means updating this switch. The alternative -- each tool declaring its own tier
via an interface -- was deferred to a future phase; the switch is small, auditable,
and the default (`TierExec`) is safe.

### Decision 3: Policy layered over mode, not replacing it

Mode (`default`, `plan`, `acceptEdits`, `bypass`) remains the base layer. Policy
(`Policy.Overrides` + `Prompter`) is a per-session overlay that can allow, deny,
or prompt on individual tools regardless of mode. The resolution order at
`permission.go:217-271` is:

1. Context cancellation -- outranks everything, even bypass
2. Classify tool / extract bash command / check destructiveness
3. Bypass mode: allow everything except explicit deny overrides
4. Destructive bash: block regardless of policy (safety wins in non-bypass modes)
5. Policy override: allow, deny, or prompt
6. Mode/tier default: plan blocks write+exec (except safe bash); acceptEdits and
   default allow all tiers (destructive bash already handled above)
7. Prompt resolution: nil Prompter denies; otherwise calls `Approve`

This layered design keeps the mode semantics stable -- plan mode still means
"read-only planning pass" -- while letting callers pin specific tools to allow or
deny. The `Prompter` interface is a seam for an interactive TTY prompt; today the
one-shot CLI passes `nil`, so prompt decisions deny.

### Decision 4: The `args *any` fix -- marshal at the frozen boundary

The old `Gate` signature was:

```go
func Gate(ctx context.Context, mode Mode, toolName string, args *any) (allow bool, reason string)
```

This violated the idiomatic-Go rules in module 12 (idiomatic Go vs TypeScript)
("No public `any` payload where a stable type is possible"). The `*any` existed because
`agent.BeforeToolCallContext.Args` at `internal/agent/types.go:103` is `any`, and
the old `commandFromArgs` used reflection and type-switching to cope with
whatever shape arrived.

The fix: `c.Args` stays `any` because `agent.BeforeToolCallContext` is frozen.
The non-frozen boundary is the permission package's public API. At
`internal/session/stack.go:96-99`, `json.Marshal(c.Args)` converts the `any` to
`json.RawMessage` before it crosses into `permission.Gate`. The new signature is:

```go
func Gate(ctx context.Context, mode Mode, toolName string, args json.RawMessage, policy Policy) (allow bool, reason string)
```

And `commandFromArgs` at `permission.go:446-453` is now a simple
`json.Unmarshal` into `map[string]any` with a `command` field lookup. The
reflection, `map[string]string`, and `*any` branches are all gone. The `any` is
confined to the frozen agent package; the permission package sees only the typed
`json.RawMessage` boundary.

### Decision 5: Config precedence -- flag > env > settings, deny wins

Three configuration channels feed the allow/deny lists, resolved at
`internal/config/config.go:374-375`:

| Precedence | Channel | Example |
|---|---|---|
| 1 (highest) | Repeatable flags | `-allow-tool read -deny-tool bash` |
| 2 | Env var (comma-separated) | `HARNESS_PERMISSION_ALLOW_TOOLS=read,grep` |
| 3 | Settings file (JSON array) | `"permission_allow_tools": ["read"]` |

`resolveToolList` at `config.go:268-277` returns the first non-empty source.
`buildPermissionPolicy` at `main.go:386-398` builds the `Policy.Overrides` map:
allow tools get `DecideAllow`, deny tools get `DecideDeny`. A tool in both lists
gets `DecideDeny` because deny is written second (deny wins). The `Prompter` is
`nil` -- the one-shot CLI is headless.

The existing `PermissionMode` flag and env path remain unchanged; mode is the
base layer and policy is the overlay.

### Decision 6: Regex-based destructive guard, not a shell parser

`IsDestructiveCommand` at `permission.go:378-394` remains a regex scanner. It
catches `rm`, `git clean`, `mkfs`, append redirect (`>>`), fork bombs, and
package-manager install commands. It does not parse shell syntax and is not a
security boundary. See Failure modes for the documented limitations.

This was already a pragmatic choice in the pre-Phase-3b system. Phase 3b did not
change it -- the destructive guard still runs at step 4 of the resolution order,
and it still wins over an allow override (safety over policy).

### Decision 7: Plan mode is a CompletionOptions gate, not a mode-only gate

The CLI has two plan-related branches:

1. **Tool blocking**: `Gate` with `ModePlan` blocks TierWrite and TierExec tools
   (safe bash excepted). This runs on every tool call.
2. **Completion control**: at `main.go:186`, `CompletionOptions.Enabled` is
   `permissionMode != permission.ModePlan`. When the mode is `plan`, the agent
   loop runs exactly one Prompt and stops.

After the single response, the CLI extracts the plan via `PlanCollector` and
prints it to stderr (`main.go:212-219`). The plan is never persisted; it is
ephemeral display output.

## Signatures and types

### Tiers and classification (`policy.go`)

```go
// policy.go:10-18
type Tier int

const (
    TierRead  Tier = iota  // tools that inspect existing data
    TierWrite              // tools that change workspace content
    TierExec               // tools that may execute arbitrary actions (safe default)
)
```

```go
// policy.go:58-67
func ClassifyTool(toolName string) Tier
```
- `toolName` -- the tool name from `c.ToolCall.Name`; unknown names return `TierExec`
- Maps `read`, `grep`, `find`, `ls`, `web_search`, `web_fetch`, `attachment` to `TierRead`;
  `write`, `edit` to `TierWrite`; everything else to `TierExec`

### Decisions and policy (`policy.go`)

```go
// policy.go:22-31
type Decision int

const (
    DecideAllow  Decision = iota
    DecideDeny
    DecidePrompt
)
```

```go
// policy.go:34-41
type PromptRequest struct {
    Tool   string  // tool name requesting approval
    Tier   Tier    // approval tier for the tool
    Reason string  // safety-override reason, if any
}
```

```go
// policy.go:44-47
type Prompter interface {
    Approve(ctx context.Context, req PromptRequest) (bool, error)
}
```
- A nil Prompter means headless: prompt decisions become denials

```go
// policy.go:50-55
type Policy struct {
    Overrides map[string]Decision  // per-tool allow, deny, or prompt
    Prompter  Prompter             // nil denies prompt decisions in headless sessions
}
```

```go
// policy.go:70-81
func ParseDecision(value string) (Decision, error)
```
- `value` -- `"allow"`, `"deny"`, or `"prompt"` (whitespace-trimmed)
- Returns an error for any unrecognized value

### Mode and parsing (`permission.go`)

```go
// permission.go:24-36
type Mode string

const (
    ModeDefault     Mode = "default"      // acceptEdits in headless; no interactive prompt surface
    ModePlan        Mode = "plan"         // block write+exec, allow read+safe-bash
    ModeAcceptEdits Mode = "acceptEdits"  // allow edits, destructive guard active
    ModeBypass      Mode = "bypass"       // allow everything except explicit deny overrides
)
```

```go
// permission.go:201-215
func ParseMode(value string) (Mode, error)
```
- `value` -- the raw string from config; blank means `ModeDefault`
- Returns an error for any string not in the four-constant set

### The gate (`permission.go`)

```go
// permission.go:217-271
func Gate(ctx context.Context, mode Mode, toolName string, args json.RawMessage, policy Policy) (allow bool, reason string)
```
- `ctx` -- checked first; cancelled context returns `(false, "Operation aborted")` regardless of mode
- `mode` -- the base permission mode
- `toolName` -- matched against `ClassifyTool` for tier; `"bash"` triggers command extraction and destructive/safe checks
- `args` -- the tool's validated arguments as `json.RawMessage`; `commandFromArgs` extracts `command` from JSON for bash; nil or malformed means empty command
- `policy` -- the per-session overlay; zero-value `Policy{}` means no overrides, no prompter
- Returns `(true, "")` when permitted, `(false, reason)` with a human-readable explanation when blocked

### Command extraction and guards (`permission.go`)

```go
// permission.go:446-453
func commandFromArgs(args json.RawMessage) string
```
- `args` -- the tool arguments as `json.RawMessage`; `json.Unmarshal` into `map[string]any`, returns the `command` string field; empty on any failure

```go
// permission.go:378-394
func IsDestructiveCommand(command string) bool
```
- `command` -- the raw bash command string
- Checks: single-output redirect, mkfs pattern, then iterates `destructivePatterns`

```go
// permission.go:284-296
func IsSafeCommand(command string) bool
```
- `command` -- the raw bash command string
- Returns `true` only when the command is NOT destructive AND matches at least one allowlisted pattern

### Plan collection (`permission.go`)

```go
// permission.go:148-151
type PlanCollector struct { /* unexported fields */ }

// permission.go:154
func NewPlanCollector() *PlanCollector

// permission.go:161-171
func (c *PlanCollector) Observe(text string) int

// permission.go:174-183
func (c *PlanCollector) Items() []TodoItem

// permission.go:186-199
func FormatPlan(items []TodoItem) string
```
- `Observe` extracts numbered todo items from text and marks completed steps via `[DONE:N]` markers; returns the count of done markers
- `Items` returns a defensive copy
- `FormatPlan` renders as a Markdown checkbox checklist
- `TodoItem` is `{Step int, Text string, Completed bool}`

### Config accessors (`config.go`)

```go
// config.go:199-207
func (c *ResolvedConfig) PermissionAllowTools() []string
func (c *ResolvedConfig) PermissionDenyTools() []string
```
- Both return defensive copies

### Policy builder (`main.go`)

```go
// main.go:383-398
func buildPermissionPolicy(allow, deny []string) permission.Policy
```
- `allow` -- tool names to allow; mapped to `DecideAllow`
- `deny` -- tool names to deny; mapped to `DecideDeny` (deny wins when a name is in both)
- Returns a `Policy` with no `Prompter` (headless one-shot)

## How it works

### 1. CLI parses mode and builds policy

`runOneShot` at `main.go:84` calls `permission.ParseMode(opts.Config.PermissionModeDefault())`.
At `main.go:89`, `buildPermissionPolicy` converts the resolved allow/deny lists
into a `permission.Policy`. The config values come from flag > env > settings >
default precedence (`config.go:374-375`).

### 2. Mode and policy flow into the stack config

At `main.go:167-168`, both `permissionMode` and `permissionPolicy` are passed as
`StackConfig.PermissionMode` and `StackConfig.PermissionPolicy`.
`StackConfig.PermissionModeFunc` is left nil, so `BuildAgentStack` wraps the
static mode in a closure at `stack.go:90-93`.

### 3. The gate hook is registered

`BuildAgentStack` at `stack.go:95-105` constructs the `beforeToolCall` closure.
It marshals `c.Args` (the frozen `any`) to `json.RawMessage`, then calls
`permission.Gate(ctx, modeFunc(), c.ToolCall.Name, rawArgs, cfg.PermissionPolicy)`.
If marshaling fails, `rawArgs` is `nil` -- `commandFromArgs` returns `""` and the
gate classifies by tier only.

### 4. The agent loop calls the hook before tool execution

At `internal/agent/executor.go:293`, after argument validation succeeds, the
agent loop calls `config.BeforeToolCall(ctx, BeforeToolCallContext{...})`. If the
hook returns a non-nil `BeforeToolCallResult` with `Block: true`, the tool is
skipped and the block reason becomes the tool result.

### 5. Gate decides: the full resolution order

`Gate` at `permission.go:217-271` follows this decision tree:

```
ctx.Err() != nil  ->  (false, "Operation aborted")

tier := ClassifyTool(toolName)
if bash: command := commandFromArgs(args); destructive := IsDestructiveCommand(command)

mode == bypass
  policy.Overrides[toolName] == DecideDeny  ->  (false, "Tool <name> denied by policy")
  else                                      ->  (true, "")

destructive  ->  (false, destructive guard reason)

policy.Overrides[toolName] exists:
  DecideAllow   ->  (true, "")
  DecideDeny    ->  (false, "Tool <name> denied by policy")
  DecidePrompt  ->  resolvePrompt(ctx, policy, PromptRequest{...})

mode == plan:
  TierRead                     ->  (true, "")
  TierWrite                    ->  (false, plan-mode reason)
  TierExec:
    bash && IsSafeCommand(cmd) ->  (true, "")
    bash                       ->  (false, "not allowlisted" reason)
    else                       ->  (false, plan-mode reason)

else  ->  (true, "")
```

`resolvePrompt` at `permission.go:273-282`: if `policy.Prompter` is nil, returns
`(false, "Tool <name> requires approval but no interactive prompt is available")`;
otherwise calls `Prompter.Approve(ctx, req)` and returns its result.

### 6. Plan mode: one-shot + plan collection

When the mode is `plan`, the CLI sets `CompletionOptions.Enabled = false` at
`main.go:186`. The agent loop runs exactly one Prompt and stops. After the single
assistant response, the CLI extracts the plan via `PlanCollector` and prints it
to stderr (`main.go:212-219`).

### Data flow summary

```
Flag (-allow-tool, -deny-tool) / Env / Settings
  -> config.ResolvedConfig.PermissionAllowTools() / PermissionDenyTools()
    -> main.buildPermissionPolicy(allow, deny)
      -> permission.Policy{Overrides: map[string]Decision}

Flag (-permission-mode) / Env / Settings
  -> config.ResolvedConfig.PermissionModeDefault()
    -> permission.ParseMode(value)
      -> permission.Mode constant

StackConfig{PermissionMode, PermissionPolicy}
  -> BuildAgentStack: modeFunc closure + beforeToolCall closure
    -> beforeToolCall: json.Marshal(c.Args) -> json.RawMessage
      -> permission.Gate(ctx, modeFunc(), toolName, rawArgs, policy)
        -> (allow, reason)
```

## Failure modes and invariants

### No interactive Prompter in the one-shot CLI

The `Prompter` interface at `policy.go:44-47` is a designed seam, but the one-shot
CLI passes `nil` at `main.go:386-398`. This means any `DecidePrompt` override
resolves to a denial: `resolvePrompt` at `permission.go:274-275` returns
`"Tool <name> requires approval but no interactive prompt is available"`.

A user who sets a tool to `prompt` in settings will see that tool blocked until
an interactive Prompter implementation ships. This is honest: the seam exists,
the policy machinery is correct, and the headless fallback is explicit rather
than a silent allow.

### Destructive bash is blocked, not prompted, in non-bypass modes

At `permission.go:238-240`, the destructive guard fires before policy overrides
are checked. Even if a user sets `-allow-tool bash`, a destructive bash command
is still blocked. The reasoning: in non-bypass modes, destructive commands are a
hard safety block, not a policy decision. Bypass mode skips the guard entirely
(`permission.go:231-236`), matching the pre-Phase-3b behavior.

A future phase could make destructive commands `DecidePrompt` rather than a hard
block, giving the interactive Prompter a chance to override. That was explicitly
deferred as a non-goal for this version.

### Cancellation outranks all mode and policy logic

`Gate` checks `ctx.Err()` first, before any mode comparison
(`permission.go:219-221`). A cancelled context returns `(false, "Operation
aborted")` even in bypass mode. This means a session cancel (Ctrl+C, timeout,
parent context cancellation) blocks all tools uniformly.

### The `any` is confined to the frozen agent boundary

`agent.BeforeToolCallContext.Args` at `internal/agent/types.go:103` remains `any`
because the agent package is frozen. The fix is the `json.Marshal` at
`stack.go:96-99`: the `any` crosses into `json.RawMessage` at the session
package, and the permission package's public API is fully typed. No `any` appears
in `policy.go` or `permission.go` signatures.

If `json.Marshal(c.Args)` fails (unlikely for anything the tool system produces),
`rawArgs` is set to `nil`. `commandFromArgs(nil)` returns `""`, so the gate
classifies by tier only -- no destructive guard triggers, which for bash means
the call may be allowed in non-plan modes. This is a graceful degradation, not a
panic.

### Regex false positives and false negatives (unchanged)

The destructive guard's regex approach has known limitations:

- **False positive on redirects inside string arguments.** `awk '{print $1 > "output.txt"}'`
  triggers the redirect scanner because `>` appears in the command string.
- **False negative on obfuscated commands.** `$(echo rm) -rf /` passes because
  the literal string `rm` does not appear. The regex does not emulate command
  substitution.
- **Package manager subcommand whitelisting is coarse.** `npm install` is
  blocked, but `npm exec rm -rf` is not.

These are documented trade-offs, not bugs. The guard is a coarse sieve, not a
security boundary.

### Empty bash command edge case

If `commandFromArgs` returns `""` (nil args, malformed JSON, or no `command`
field), `IsDestructiveCommand("")` returns `false` and `IsSafeCommand("")` returns
`false`. In plan mode, this means bash is blocked with "Plan mode: command blocked
(not allowlisted)." In non-plan modes, it means bash is allowed -- `Gate` falls
through to `return true, ""`. The tool execution layer may then reject the empty
command, but the permission gate does not.

### Plan mode + CompletionOptions false: the agent never loops

When `CompletionOptions.Enabled` is `false`, the `RunCompletionLoop` function
exits after one Prompt call. The plan collector runs, but there is no path back
into a tool-calling loop. If the model's plan response is empty, the collector
produces nothing and the CLI prints nothing. There is no retry.

## TypeScript to Go

### TS: ad-hoc permission checks scattered across tool implementations

In a TypeScript agent, permission checks are typically ad-hoc. Each tool's
`execute` function starts with a guard:

```ts
async function executeBash(args: { command: string }, context: AgentContext) {
    if (context.mode === "plan") {
        return { blocked: true, reason: "Plan mode: bash is disabled" };
    }
    if (isDestructive(args.command)) {
        return { blocked: true, reason: "Destructive command blocked" };
    }
    return exec(args.command);
}
```

Each tool duplicates the same structure. Adding a new mode means touching every
tool file. A tool that forgets its guard becomes a silent bypass.

### Go: A single typed gate registered as a hook

The Go harness inverts this: tools know nothing about permissions. The gate is a
single function registered once in `BuildAgentStack`. Every tool passes through
it automatically. Adding a tool to a tier is one line in `ClassifyTool`. The gate
is testable in isolation (`internal/permission/permission_test.go`).

### The `any` boundary: TS `unknown` vs Go interfaces

The Phase 3b fix is the Go-idiomatic resolution of the TS-Go tension around
dynamic argument shapes. The TypeScript reference can use `unknown` with runtime
type guards:

```ts
function gate(args: unknown): boolean {
    if (typeof args === "object" && args !== null && "command" in args) {
        return isDestructive((args as { command: string }).command);
    }
    return false;
}
```

TypeScript's `unknown` forces a type guard before use -- the compiler won't let
you access `.command` without narrowing. Go's `any` has no such guard; it
disables compile-time checking entirely.

The fix: keep the `any` at the frozen boundary (`agent.BeforeToolCallContext.Args`)
and marshal to `json.RawMessage` before it enters the permission package. This
is the same pattern used across the harness: `json.RawMessage` is the sanctioned
"opaque payload" type for domain boundaries. It says "this is JSON, and the
receiver decides what to do with it" without erasing type information downstream.

`commandFromArgs` at `permission.go:446-453` is now a focused three-line function:
unmarshal into a map, extract the `command` string, return it. The old reflection
and type-switching branches are gone. The compiler cannot verify the JSON shape,
but the runtime contract is simple and the failure mode (empty string) is explicit.

## Where it lives

| Path | What |
|---|---|
| `internal/permission/policy.go:10-18` | `Tier` type and `TierRead`/`TierWrite`/`TierExec` constants |
| `internal/permission/policy.go:22-31` | `Decision` type and `DecideAllow`/`DecideDeny`/`DecidePrompt` constants |
| `internal/permission/policy.go:34-41` | `PromptRequest` struct |
| `internal/permission/policy.go:44-47` | `Prompter` interface |
| `internal/permission/policy.go:50-55` | `Policy` struct with `Overrides` and `Prompter` |
| `internal/permission/policy.go:58-67` | `ClassifyTool` -- name-to-tier mapping |
| `internal/permission/policy.go:70-81` | `ParseDecision` -- "allow"/"deny"/"prompt" string parser |
| `internal/permission/permission.go:24-36` | `Mode` and four mode constants |
| `internal/permission/permission.go:201-215` | `ParseMode` |
| `internal/permission/permission.go:217-271` | `Gate` -- the full resolution engine |
| `internal/permission/permission.go:273-282` | `resolvePrompt` -- headless denial or Prompter call |
| `internal/permission/permission.go:284-296` | `IsSafeCommand` |
| `internal/permission/permission.go:378-394` | `IsDestructiveCommand` |
| `internal/permission/permission.go:446-453` | `commandFromArgs` -- simple JSON extract |
| `internal/permission/permission.go:38-125` | `destructivePatterns` and `safePatterns` regex tables |
| `internal/permission/permission.go:148-199` | `PlanCollector`, `TodoItem`, `NewPlanCollector`, `Observe`, `Items`, `FormatPlan` |
| `internal/permission/permission_test.go` | Decision-table coverage: tiers, overrides, headless prompt, cancellation, destructive patterns |
| `internal/session/stack.go:42-49` | `PermissionMode`, `PermissionModeFunc`, `PermissionPolicy` in `StackConfig` |
| `internal/session/stack.go:95-105` | `beforeToolCall` closure with `json.Marshal` of `c.Args` |
| `internal/session/stack.go:164-169` | `AgentOptions` wiring with `BeforeToolCall` |
| `internal/config/config.go:87-88` | `PermissionAllowTools`/`PermissionDenyTools` in `FlagValues` |
| `internal/config/config.go:141-142` | `permissionAllowTools`/`permissionDenyTools` in `ResolvedConfig` |
| `internal/config/config.go:199-207` | `PermissionAllowTools()`/`PermissionDenyTools()` accessors |
| `internal/config/config.go:241-242` | `PermissionAllowTools`/`PermissionDenyTools` in `fileSettings` |
| `internal/config/config.go:268-277` | `resolveToolList` -- flag > env > settings precedence |
| `internal/config/config.go:374-375` | `resolveToolList` calls in `Resolve()` |
| `cmd/harness/main.go:245-246` | `-allow-tool` / `-deny-tool` repeatable flags |
| `cmd/harness/main.go:89` | `buildPermissionPolicy` call |
| `cmd/harness/main.go:167-168` | `PermissionMode` + `PermissionPolicy` passed to `StackConfig` |
| `cmd/harness/main.go:186` | Plan mode disables `CompletionOptions.Enabled` |
| `cmd/harness/main.go:212-219` | Plan collection and display |
| `cmd/harness/main.go:383-398` | `buildPermissionPolicy` -- deny wins when both |
| `internal/agent/types.go:99-104` | `BeforeToolCallContext` with `Args any` (frozen boundary) |
