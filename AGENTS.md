# Code Amigo (codamigo)

codamigo is a Go code intelligence tool: it walks a source tree, chunks files into semantically coherent pieces via tree-sitter ASTs, embeds chunks either in-process with a local GoMLX model or via any OpenAI-compatible provider, stores them in a local sqlite-vec database, and exposes hybrid semantic search (KNN + BM25) via a CLI and an MCP stdio server.

## Commands

CGo is required (tree-sitter grammars include C sources). Ensure `cc` is on `PATH`, or set `CC`.

```sh
make build          # build all packages
make test           # run all tests
make test-config    # single-package (also: test-store, test-walker,
                    #   test-indexer, test-query, test-mcp,
                    #   test-localembed, test-arch)
make test-race      # race detector across all packages
make fmt            # format (go fmt)
make vet            # static analysis
make lint           # golangci-lint
make docker         # CPU image (docker-cuda is unverified — see Dockerfile.cuda)
```

`localembed` inference tests skip unless the model is downloaded; set
`CODAMIGO_TEST_MODELS_ROOT` to point at one, or run `codamigo download-model`.
Under `-race` the pure-Go compute backend is also skipped — it trips `checkptr`
inside GoMLX's own unsafe matmul, which aborts the whole test binary. The XLA
backend still covers the concurrency paths.

Single named test: `make run-test PKG=./indexer/... TEST=TestIndex_Basic`

All `make` targets pass `-tags sqlite_fts5`. Running `go test` directly requires adding that tag.

`make bench-localembed` runs `BenchmarkEmbedBatch`, which reproduces the
throughput table in README.md and `localembed/backend.go`. It needs the model
downloaded and takes a few minutes — the pure-Go rows are slow by design. Re-run
it before changing any published speed claim.

Specialized one-off commands (no `make` target): `go build -gcflags='-m'` (escape analysis), `go fix ./...` (Go version upgrade).

## Package structure

Dependency order is strict and one-directional — no cycles.

```
config              store              localembed
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
                └── langs/    (language configs + edge rules, CGo)
              github.com/ieshan/go-embedder
                ├── embedder/ (Embedder interface)
                └── openai/   (OpenAI-compatible client)
              github.com/gomlx/{gomlx,compute,go-huggingface,go-xla}
                (imported only by localembed and cmd/codamigo)
```

`internal/arch` enforces this order as a test (`make test-arch`). It is not
documentation that can drift: adding an edge means widening the allow-list there
and the diagram above together.

- `github.com/ieshan/go-code-chunker` — external module providing `chunker/` (cAST algorithm) and `langs/` (per-language configs with CGo grammars); imported only by `cmd/codamigo`
- `github.com/ieshan/go-embedder` — external module providing `embedder.Embedder` interface and `openai.Client` implementation; imported by `indexer`, `query`, and `cmd/codamigo`
- `config/` — `Config` struct; no internal imports
- `localembed/` — in-process embedding via GoMLX; second `embedder.Embedder` implementation alongside `openai.Client`, selected by `embedding_provider: local`. Imported only by `cmd/codamigo`, and deliberately does **not** import `config`
- `store/` — `Store` interface + sqlite-vec; also owns the code-graph tables (`edges`, `file_imports`); never imports `go-code-chunker/chunker`
- `walker/` — recursive FS walk + gitignore + include/exclude filtering
- `watcher/` — fsnotify + poll watcher implementations
- `indexer/` — walk → chunk → embed → store pipeline; maps `chunker.Edge` → `store.Edge`/`store.Import`
- `query/` — embed query, hybrid KNN + BM25 search, repo map, code-graph traversal (`Callers`/`Callees`/`Impact`) with query-time target resolution
- `mcp/` — MCP stdio server; `search`, `get_map`, `get_callers`, `get_callees`, `get_impact` tools
- `cmd/codamigo/` — CLI entry point; only place that imports `go-code-chunker/langs`

## Architectural rules

- **No circular imports.** Add a shared interface instead.
- **`go-code-chunker/langs` only imported by `cmd/`.** Inject `*chunker.Chunker` everywhere else.
- **`store` never imports `go-code-chunker/chunker`.** Map `chunker.Chunk` → `store.Record` in `indexer`.
- **Config passed at construction time.** No package reads global config state. `localembed` takes an `Options` struct and never imports `config`; `internal/arch` enforces that.
- **Only `embedding_provider: local` is special-cased.** Every other value routes to the OpenAI-compatible client, so there is no provider allow-list to keep in sync — adding a provider name is a config change, not a code change.
- **Store dimension comes from `emb.Dim()`, never `cfg.EmbeddingDimensions`.** For the local provider the model is the source of truth, and `Defaults()` sets 1536; using the configured value made `doctor` report a spurious store failure for a 384-dim model. See `storeDim` in `doctor_cmd.go`.
- **Embedders that hold native resources are closed.** `embedder.Embedder` stays at four methods; `*localembed.Embedder` additionally implements `io.Closer`, and every construction site pairs `newEmbedder` with `defer closeEmbedder`. GoMLX buffers are freed explicitly, not by the GC.
- **`newEmbedder` never returns a `WithPrefix` view.** A view's `Close` is a no-op by design, so returning one makes the caller's `defer closeEmbedder` inert and the compute backend is never finalized. The query side comes from `localembed.Options.ApplyQueryPrefix`, which returns the owner with the prefix applied. `WithPrefix` is only for `queryEmbedderFor`, where the caller already owns the document embedder the view shares.
- **Required dependencies are positional; everything with a default is an `Option`.** `indexer.New` and `mcp.NewServer` take only what the type cannot work without, then `...Option`. A new knob is a `WithXxx` function, never a new parameter — that way adding one does not touch a single existing call site, and call sites stay readable instead of a run of bare ints and nils.
- **Constructors reject nil required dependencies at construction, not first use.** Go cannot express "required" in a type, so `indexer.New` and `query.New` panic immediately with `<pkg>.New: <dep> must not be nil`. Every guard has a test. Constructors that do I/O (`walker.New`, `watcher.New`, `store.NewSQLiteStore`) return an error instead; `mcp.NewServer`'s dependencies are all genuinely optional and it degrades gracefully.
- **No packages named `util`, `common`, `helpers`, or `shared`.** Use a domain name.
- **Indexer must not buffer paths.** Feed directly from `walker.Walk(ctx)` into the errgroup — never collect into `[]string` first.
- **Return an untyped `nil` interface on error, never a typed nil pointer.** `return openai.New(...)` from a function returning `embedder.Embedder` hands back a non-nil interface wrapping a nil pointer, so a caller's `!= nil` guard passes and the next method call panics. Same trap as the `indexer.Progress` conversion in `indexCmd`.
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
