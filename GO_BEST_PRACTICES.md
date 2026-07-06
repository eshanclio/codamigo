# MEMORY.md

## Purpose
Portable Go memory for AI coding agents.
This file is intentionally **project-agnostic** and can be dropped into any Go repository to provide up-to-date (through Go 1.26) guidance on language features, engineering practices, tooling, API design, security, and error handling.

---

## Quick Agent Defaults (apply unless repo says otherwise)
1. Respect `go.mod` as source of truth (module path, Go version, dependency graph).
2. Keep changes idiomatic and minimal; preserve public API contracts unless asked to break them.
3. Always run formatting + tests after code changes.
4. Prefer explicit, inspectable error handling over panic.
5. Use context correctly in boundaries and concurrent flows.
6. If behavior changes, add/adjust tests first or alongside implementation.

---

## Go Language & Stdlib Features to Know (1.20 → 1.26)

### Go 1.20
- Multi-error wrapping improvements:
  - `errors.Join(...)`
  - `fmt.Errorf` supports multiple `%w`
  - `errors.Is`/`errors.As` traverse multi-error trees
- `context` cancellation causes: `WithCancelCause`, `Cause`
- `net/http` `ResponseController`

### Go 1.21
- Built-ins: `min`, `max`, `clear`
- Structured logging in stdlib: `log/slog`
- Expanded context utilities: `WithTimeoutCause`, `WithDeadlineCause`, `AfterFunc`, `WithoutCancel`
- Stronger toolchain/version compatibility controls

### Go 1.22
- `for` loop variable semantics fixed (per-iteration capture safety)
- Integer `range`
- Expanded `net/http` ServeMux patterns (method/wildcard style)

### Go 1.23
- `range` over iterator functions
- `iter` package + iterator helpers in `maps`/`slices`
- Timer behavior improvements for correctness/GC

### Go 1.24
- Generic type aliases fully supported
- `runtime.AddCleanup` introduced (preferred over many finalizer uses)
- `testing.B.Loop` for benchmarks
- `os.Root` / `os.OpenRoot` for safer filesystem scoping

### Go 1.25
- No major language syntax changes
- Runtime/tooling improvements (e.g., `runtime/trace.FlightRecorder`)
- Green Tea GC available as experiment

### Go 1.26
- `new(...)` accepts expression initializers
- More expressive generic constraints
- `go fix` modernizers revamp
- Green Tea GC enabled by default
- `errors.AsType[E error]` for typed generic extraction

---

## Core Best Practices

## 1) Modules and dependency hygiene
- Use Go modules (`go.mod`, `go.sum`) as canonical dependency state.
- Prefer `go get` / `go mod tidy` over manual editing for dependency changes.
- Keep `go.mod` minimal and accurate; remove dead dependencies regularly.
- Understand and use `replace`/`exclude`/`retract` intentionally.
- Treat `go` directive as minimum supported toolchain behavior contract.

## 2) Formatting and style
- Enforce canonical formatting with `gofmt` (`go fmt ./...`).
- Keep code simple, explicit, and readable over clever.
- Follow standard Go naming and export conventions.

## 3) Context usage
- `context.Context` should be first parameter where required.
- Never pass nil context (`context.TODO()` if needed).
- Don’t store context in long-lived structs unless there is a documented compatibility reason.
- Context values are for request-scoped metadata, not optional argument transport.
- Honor cancellation and deadlines in I/O/concurrency paths.

## 4) Error hygiene
- Never ignore returned errors in normal control flow.
- Avoid panic for expected failures.
- Keep error strings lowercase and punctuation-light.
- Use wrapping intentionally to define API behavior.

## 5) API design discipline
- Public APIs should have stable, documented behavior.
- Exported identifiers should have doc comments.
- Keep interfaces in consumer packages when possible.
- Prefer small, composable types and functions.
- Preserve backward compatibility unless explicitly changing major behavior.

## 6) Concurrency discipline
- Every goroutine should have a clear stop condition.
- Avoid leaks via blocked sends/receives.
- Use context cancellation and channel closure patterns deliberately.
- Be explicit with ownership of shared mutable state.

---

## Modern Error Handling Playbook

## A) Inspect errors with `errors.Is` / `errors.As`
Prefer semantic checks over string matching or direct equality on wrapped errors.

```go
var ErrNotFound = errors.New("not found")

func lookup(id string) error {
	if id == "" {
		return fmt.Errorf("lookup %q: %w", id, ErrNotFound)
	}
	return nil
}

func handle(err error) bool {
	return errors.Is(err, ErrNotFound)
}
```

## B) Use `errors.Join` for multi-failure flows
Useful for batch operations, cleanup failures, fan-out execution.

```go
err := errors.Join(errA, errB, errC)
if errors.Is(err, errB) {
	// true
}
```

## C) Use typed extraction (`errors.AsType` in Go 1.26)

```go
if pe, ok := errors.AsType[*os.PathError](err); ok {
	_ = pe.Path
}
```

## D) Wrapping policy = API contract
- Use `%w` when callers should be able to inspect causes.
- Avoid wrapping internals when you do not want to expose implementation details.

## E) Context-aware cancellation errors
Use cause-aware context APIs and inspect with `context.Cause(ctx)` where cancellation reasons matter.

---

## Testing & Quality Guidance

## 1) Unit/integration testing
- Use table-driven tests with `t.Run` for coverage and maintainability.
- Test behavior and boundaries, not just happy paths.
- Keep tests deterministic and isolated.

## 2) Coverage
- Unit coverage: `go test -cover ./...` and `-coverprofile` when needed.
- Integration coverage (Go 1.20+): build with `-cover`, collect with `GOCOVERDIR`, analyze with `go tool covdata`.

## 3) Race detection
- Run `go test -race ./...` for concurrency-sensitive changes.

## 4) Fuzzing
- Use fuzz tests (`FuzzXxx`) for parser/decoder/validation/string-processing boundaries.
- Keep generated regression corpus under `testdata/fuzz/...`.

## 5) Benchmarks
- Use `BenchmarkXxx` and benchmark only stable scenarios.
- Use `testing.B.Loop` when appropriate (Go 1.24+).

## 6) Static checks
- Run `go vet ./...` for suspicious constructs.

---

## Tooling & Diagnostics

## Daily dev commands (generic baseline)
```bash
go fmt ./...
go vet ./...
go test ./...
go test -race ./...
go test -cover ./...
```

## Profiling and tracing
- CPU/heap/block profiling: `go test -cpuprofile/-memprofile` + `go tool pprof`.
- Runtime traces: `go test -trace` + `go tool trace`.
- Prefer measurement-driven optimization, not intuition-only changes.

## Modernization
- Use `go fix` (Go 1.26 modernizers) when migrating codebases/toolchain idioms.

---

## Security & Supply Chain Guidance
- Keep Go toolchain and dependencies up to date.
- Run vulnerability scanning routinely with `govulncheck ./...`.
- Treat external input as untrusted; validate at boundaries.
- For filesystem boundary constraints, consider `os.OpenRoot` / `os.Root` (Go 1.24+) where applicable.
- Use fuzzing and race detector as part of secure engineering baseline.

---

## Agent Change Checklist (project-agnostic)
Before coding:
- Read `go.mod`, key package docs, and existing tests.
- Identify whether change affects public API, behavior contracts, or compatibility.

While coding:
- Keep deltas minimal and style-consistent.
- Add tests for new behavior and edge cases.
- Be explicit about error wrapping policy.
- Ensure context and goroutine lifecycle correctness.

Before finishing:
- `go fmt ./...`
- `go vet ./...`
- `go test ./...`
- Run `-race` for concurrency-related changes.
- Run targeted benchmarks/profiling only when performance claims are made.

---

## Canonical Sources
- Go release notes: https://go.dev/doc/devel/release
- Go 1.20: https://go.dev/doc/go1.20
- Go 1.21: https://go.dev/doc/go1.21
- Go 1.22: https://go.dev/doc/go1.22
- Go 1.23: https://go.dev/doc/go1.23
- Go 1.24: https://go.dev/doc/go1.24
- Go 1.25: https://go.dev/doc/go1.25
- Go 1.26: https://go.dev/doc/go1.26
- Modules: https://go.dev/doc/modules/managing-dependencies
- go.mod reference: https://go.dev/doc/modules/gomod-ref
- `errors`: https://pkg.go.dev/errors
- `context`: https://pkg.go.dev/context
- `testing`: https://pkg.go.dev/testing
- Race detector: https://go.dev/doc/articles/race_detector
- Fuzzing: https://go.dev/doc/security/fuzz/
- Coverage: https://go.dev/doc/build-cover
- Diagnostics: https://go.dev/doc/diagnostics
- `go vet`: https://pkg.go.dev/cmd/vet
- `gofmt`: https://pkg.go.dev/cmd/gofmt
- Vulnerability management: https://go.dev/doc/security/vuln/
- `govulncheck`: https://go.dev/doc/tutorial/govulncheck
- Doc comments guidance: https://go.dev/doc/comment
- Go review guidance: https://go.dev/wiki/CodeReviewComments
