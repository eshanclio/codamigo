# Code Amigo (codamigo)

codamigo is a Go code intelligence tool: it walks a source tree, chunks files into semantically coherent pieces via tree-sitter ASTs, embeds chunks with any OpenAI-compatible provider, stores them in a local sqlite-vec database, and exposes hybrid semantic search (KNN + BM25) via a CLI and an MCP stdio server.

## Commands

CGo is required (tree-sitter grammars include C sources). Ensure `cc` is on `PATH`, or set `CC`.

```sh
make build          # build all packages
make test           # run all tests
make test-config    # single-package (also: test-store, test-walker,
                    #   test-indexer, test-query, test-mcp)
make fmt            # format (go fmt)
make vet            # static analysis
make lint           # golangci-lint
```

Single named test: `make run-test PKG=./indexer/... TEST=TestIndex_Basic`

All `make` targets pass `-tags sqlite_fts5`. Running `go test` directly requires adding that tag.

Specialized one-off commands (no `make` target): `go build -gcflags='-m'` (escape analysis), `go fix ./...` (Go version upgrade), `go test -bench ./...` (benchmarks).

## Package structure

Dependency order is strict and one-directional — no cycles.

```
config              store
                  walker ────────────────┐
                  watcher ───────────────┤
                  indexer ◄──────────────┤
                  query ◄── store        │
                  mcp ◄── indexer,       │
                          query,         │
                          store,         │
                          watcher        │
                  cmd/codamigo ──────────┘
                        │
                        ▼
              github.com/ieshan/go-code-chunker
                ├── chunker/  (cAST algorithm)
                └── langs/    (language configs, CGo)
              github.com/ieshan/go-embedder
                ├── embedder/ (Embedder interface)
                └── openai/   (OpenAI-compatible client)
```

- `github.com/ieshan/go-code-chunker` — external module providing `chunker/` (cAST algorithm) and `langs/` (per-language configs with CGo grammars); imported only by `cmd/codamigo`
- `github.com/ieshan/go-embedder` — external module providing `embedder.Embedder` interface and `openai.Client` implementation; imported by `indexer`, `query`, and `cmd/codamigo`
- `config/` — `Config` struct; no internal imports
- `store/` — `Store` interface + sqlite-vec; never imports `go-code-chunker/chunker`
- `walker/` — recursive FS walk + gitignore + include/exclude filtering
- `watcher/` — fsnotify + poll watcher implementations
- `indexer/` — walk → chunk → embed → store pipeline
- `query/` — embed query, hybrid KNN + BM25 search, repo map
- `mcp/` — MCP stdio server; `search` and `get_map` tools
- `cmd/codamigo/` — CLI entry point; only place that imports `go-code-chunker/langs`

## Architectural rules

- **No circular imports.** Add a shared interface instead.
- **`go-code-chunker/langs` only imported by `cmd/`.** Inject `*chunker.Chunker` everywhere else.
- **`store` never imports `go-code-chunker/chunker`.** Map `chunker.Chunk` → `store.Record` in `indexer`.
- **Config passed at construction time.** No package reads global config state.
- **No packages named `util`, `common`, `helpers`, or `shared`.** Use a domain name.
- **Indexer must not buffer paths.** Feed directly from `walker.Walk(ctx)` into the errgroup — never collect into `[]string` first.
- **Do not create git worktrees or commit unless explicitly asked.**

## Graceful shutdown

Every long-running operation accepts `context.Context` and must respect cancellation promptly.

- **Signal handling lives only in `cmd/codamigo`.** No other package installs signal handlers or creates a background context.
- **No package creates its own background context.** The root context flows unchanged: `cmd` → `indexer.Index` → `walker.Walk` / `embedder.Embed` → `query.Search` → `mcp.Server.Serve` → `watcher.Watch`.
- **`indexer.Index` returns `nil` on context cancellation.** Partial progress already written to the store is valid. At `g.Wait()`: if it errors but `ctx.Err() != nil`, return `nil`.
- **`store.Close()` is always deferred** in `cmd/codamigo` — checkpoints the WAL regardless of completion or cancellation.
- **Exit code 0** for clean completion and signal-triggered shutdown; non-zero for errors only.
- Per-layer: `walker` checks `ctx.Done()` between entries; `indexer` between files (not mid-file); `embedder` at rate-limiter/HTTP/retry; `watcher` between ticks; `mcp` drains pending responses before returning.

## Go 1.26+ conventions

**Language:** `for i := range N`; `iter.Seq[V]`/`iter.Seq2[K,V]` for iterators; `min`/`max`/`clear` builtins; `math/rand/v2`; `slices`/`maps` packages; `sync.OnceValue`/`sync.OnceFunc`.

**Errors:** `%w` for inspectable errors, `%v` otherwise; `errors.Is`/`errors.As` (never raw `==`); handle once — log or return, not both; `var ErrFoo = errors.New(...)`.

**Logging:** `log/slog` only; `slog.NewTextHandler` in CLIs, `slog.NewJSONHandler` in services; `slog.DiscardHandler` in tests.

**Security:** `os.Root` for all directory-scoped FS ops; `crypto/rand` for secrets; `exec.Command` with separate args — never shell strings from user input.

**Concurrency:** `context.Context` first argument; `defer cancel()` immediately after deadline/timeout context; `errgroup.SetLimit(n)`; `time.NewTimer` + `defer timer.Stop()` — never bare `time.After` in a `select` with `ctx.Done()`.

## Testing

- Table-driven tests with `t.Run`; real sqlite-vec DB (`:memory:` fine for most tests); no DB mocks.
- `testing/synctest` for watcher tests — no `time.Sleep` or real wall-clock waits.
- `t.Context()` in tests; `t.Helper()` in helpers; fuzz functions in `*_test.go` as `func FuzzXxx(f *testing.F)`.

## Boundaries

**Never:** panic on user-controlled input (only on programmer errors — nil deps, violated invariants); expose raw Go error strings, filesystem paths, or DB errors to MCP clients (log server-side, return a generic message); skip the file-level content-hash check in the indexer.

**Always:** `defer rows.Close()` immediately after `QueryContext`; `defer cancel()` immediately after any context with a deadline or timeout; validate path parameters at system boundaries — reject absolute paths, `..` prefixes, and the `.` result of `filepath.Clean`.
