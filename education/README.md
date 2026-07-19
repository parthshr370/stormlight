# Harness Education

A guided tour of how this harness actually works, one subsystem at a time, written
for an engineer who wants the mental model **and** the engineering decisions behind
each feature. Not a folder listing, not line-by-line narration. The kind of
explanation you'd get from someone who sat with the code, built it, and hit the
sharp edges.

Read it like a book. Each module traces one subsystem from the pain it removes to
the shape of the solution, shows the real function signatures (go-doc style, no
bodies), follows the data, names the ways it breaks, and calls out where a
TypeScript agent would do it one way but Go forces or rewards another.

## Why this exists

We are porting behavior from a TypeScript reference agent into idiomatic Go and
selling the result. Two things make that hard and worth documenting:

1. **The decisions are the value.** Anyone can read a signature. The judgment —
   why a retry decorator instead of a frozen-loop edit, why `json.RawMessage`
   instead of `any`, why we never read a live `Partial` pointer — is what turns
   code into understanding. Every module leads with the *why*.
2. **TypeScript and Go disagree.** Single-threaded event loop vs goroutines.
   Structural typing vs interfaces. `unknown` vs `any` vs typed unions. A direct
   translation produces subtle bugs (we shipped one and caught it under `-race`).
   Each module has a **TypeScript to Go** section for exactly these pivots.

## How to read a module

Every module follows the same seven-part shape so you always know where to look:

1. **The problem** — traced backward. What breaks or is painful without this
   subsystem. Understand the pain before the solution.
2. **Key decisions and the thinking process** — the design we chose, the
   alternatives we rejected, and *why*. The engineering judgment.
3. **Signatures and types** — the contract, go-doc style:
   `func Name(param1 T1, param2 T2) (Result, error)`, with one line per parameter
   explaining what it contributes and what the code assumes about it. Bodies are
   shown only when one small block is the whole point.
4. **How it works** — the execution flow and data flow. When the shape changed,
   a Before / After / Data-flow pass.
5. **Failure modes and invariants** — the edge cases, the race conditions, the
   contracts each function assumes, and how we keep them from breaking.
6. **TypeScript to Go** — what the reference did, what we do instead, and why the
   language forces or rewards the difference.
7. **Where it lives** — the exact files and key symbols, so you can jump in.

## Curriculum

Read in order the first time; each module assumes the earlier ones.

| # | Module | What you learn |
|---|---|---|
| 00 | Architecture and startup | The single-module layout, `cmd/harness` boot, how config to roles to session to the agent loop is wired |
| 01 | The streaming engine and agent loop | `StreamEvent`, `AssistantBuilder`, `StreamFn`, and the tool-calling loop that is the heart of the harness |
| 02 | Providers and SSE transport | The provider abstraction, Anthropic/OpenAI/Google, the SSE parser, and the `OnResponse` status/header hook |
| 03 | Tools and hash-anchored editing | `AgentTool`, the registry, the read tool's structural outline, hash-anchored edits, and the `json.RawMessage` result boundary |
| 04 | Config and model roles | Immutable `Resolve`, precedence (flag > env > settings > default), and how model roles route to providers |
| 05 | Skills, rules, and context discovery | `skill://` resolution with `os.Root` containment, the repo walk, `@file` imports, and sticky `RULES.md` |
| 06 | Retry, backoff, and typed errors | The retry decorator, dependency injection for time/sleep/randomness, classification precedence, and typed error taxonomy |
| 07 | Concurrency and race conditions | Goroutines and channels in the stream, the live-`Partial` race we shipped and fixed, ownership discipline, and cancellation |
| 08 | Compaction | Why and when we summarize history, and how compaction stays separate from retry |
| 09 | The prompt system | The typed system-prompt template, capability projection, and why the prompt never names a tool the registry lacks |
| 10 | Permissions and approval | Permission modes, the destructive-command guard, and the tool gate (evolving in Phase 3 into read/write/exec tiers) |
| 12 | Idiomatic Go vs TypeScript | The capstone: consumer-defined interfaces, accept-interface/return-struct, typed errors, no `any`, DI, defensive copies, `os.Root`, `go:embed`, saturating arithmetic |

Module 07 is the worked exemplar: it sets the voice and depth every module aims for.

## Conventions

- **Signature-first.** Lead with the contract; drop into a body only when one block
  carries the idea. Borrowed from how good godoc reads.
- **Trace backward.** Explain the pain, then the fix. "What would break if this
  didn't exist?" is a better question than "how would I design this?".
- **Grounded.** Every signature and claim points at a real `file:line`. Nothing is
  invented. If code and a doc disagree, the module says so.
- **Plain words.** Senior-to-senior at a whiteboard. No filler, no line-by-line.

## Living document

Each module traces one subsystem from its compiled behavior and its design
decisions. Engine-core subsystems (engine, providers, transport, agent loop) are
documented from how the code actually behaves; feature subsystems are documented
with the reasoning behind their design.
