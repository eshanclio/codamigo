# Local embedding provider: GoMLX + go-huggingface

Status: implemented. Measurements in this document were taken on darwin/arm64
(M-series, 10 cores), Go 1.26.5, `CGO_ENABLED=1`, with
`go-huggingface v0.4.1`, `gomlx v0.28.2`, `compute v0.1.2`, `go-xla v0.4.1`.

## Problem

`newEmbedder` returned `*openai.Client` unconditionally, so every `index`,
`search`, `map`, `callers`/`callees`/`impact`, `serve`, and `doctor` run needed a
reachable HTTP endpoint and usually an API key. That blocked offline use, sent
source code to a third party, and cost money per index.

`embedding_provider` already existed in `config.Config` but was inert — only
`Defaults`, `Merge`, and `Validate` read it.

## Decision

Add a second `embedder.Embedder` implementation, `localembed`, that runs a
sentence-transformers model in-process via GoMLX. Select it with
`embedding_provider: local`. Weights are downloaded from HuggingFace by an
explicit `codamigo download-model` into `~/.codamigo/models`, never implicitly.

Constraints that shaped the design:

- **`localembed` does not import `config`.** Everything arrives through
  `Options`. `internal/arch/layering_test.go` enforces this, so the AGENTS.md
  rule is mechanical rather than remembered.
- **Only `local` is special-cased.** No provider allow-list, because one would
  break existing configs naming `voyage` (a value the field's own doc comment
  suggests). `config_test.go` has a regression test for this.
- **The model, not the config, is the source of truth for dimensionality.**
  `Dim()` reads `hfModel.Config.HiddenSize`.

## What the dependency spike established

Everything below was measured, not assumed. Four findings changed the design
from the original plan.

### The HuggingFace token is not required

`hub.New("BAAI/bge-small-en-v1.5")` + `DownloadInfo` + the full 127 MB weight
download all succeed **anonymously**, with `HF_TOKEN` and `CODAMIGO_HF_TOKEN`
unset. Verified again end to end through `codamigo download-model` itself.

The original plan made a token mandatory with an `ErrMissingHFToken` pre-flight
check. That was invented friction. The token is now optional and used only for
gated or private repositories, and the missing-token hint fires only on a 401/403
*when no token was supplied* — telling someone to set a token they already set
sends them looking in the wrong place.

### XLA-CPU works on macOS and pure Go is too slow to be a default

`compute.List()` = `[go xla stablehlo hlo shlo]`. Both `xla` and `xla:cpu`
construct on darwin/arm64; `xla:cuda` correctly reports the plugin as missing.

Throughput, `bge-small-en-v1.5`, batch 8, steady state, embeddings/second. These
are the figures from `BenchmarkEmbedBatch`, which supersede the spike's ad-hoc
measurement — see *Correcting the throughput table* below:

| seqLen | `go` | `xla` | speedup |
|-------:|-----:|------:|--------:|
| 32 | 7.5 | 186.6 | 25× |
| 128 | 3.5 | 51.8 | 15× |
| 256 | 1.5 | 18.2 | 12× |
| 512 | 0.5 | 7.0 | 15× |

Realistic code chunks sit at seq 128–512, where pure Go manages 0.5–3.5/sec.
So `go` is a correctness fallback, **not** the default, and the original plan's
`compute.DefaultConfig = "go"` would have handed every user an unusable tool.

`auto` therefore tries `xla:cuda → xla → go`. The concern that motivated the
pure-Go default was real — GoMLX's own default fetched a CPU plugin and took 5.6s
on first construction — but the fix is `GOMLX_NO_AUTO_INSTALL=1`, which makes a
missing plugin fail fast into the next candidate, not giving up XLA's speed.

### Deriving `seqLen` in-graph is mandatory, not an optimization

`seqLen` feeds the **attention mask** (`CallOptions{SeqLen: ...}`), not just
pooling. Passing `nil` — as go-huggingface's own example does — silently degrades
quality once padding is present. Cosine against the Python reference for a
28-token prompt:

| padded to | `seqLen=nil` | derived `seqLen` |
|----------:|-------------:|-----------------:|
| 28 (none) | 1.000000 | 1.000000 |
| 64 | **0.928459** | 1.000000 |
| 128 | **0.867492** | 1.000000 |

No error is raised on either path. `bge` uses CLS pooling, so this is *not*
limited to mean-pooling models as one might assume.

Consequence for the tests: the golden test **must** include a padded case.
Replicating the upstream example (batch 1, no padding) would pass while
production returned 0.87-quality vectors. `TestGoldenVectors_PaddedBatch` exists
for exactly this.

### Two safety concerns were unfounded; a third was real and undocumented

- **`SetMaxCache` does not evict — it hard-errors** when full
  (`exec.go`: "maximum cache size of N reached ... cannot create another graph").
  So "does eviction leak device buffers?" is moot: nothing is ever evicted, and
  `SetMaxCache(len(closedShapeSet))` becomes a **correctness guard** — a
  `bucketize` regression that emits an unplanned shape fails loudly.
- **Eager precompilation is unnecessary.** It costs 6.19s for 21 shapes on XLA
  (51ms on `go`), which every `search` or `callers` invocation would pay. Its
  other justification — that a panicking first compile could strand `pending`
  waiters — is void: `buildAndCompileGraph` wraps construction in
  `exceptions.TryCatch`, so a GoMLX `Panicf` becomes an error and `close(pc.done)`
  still runs. Compilation is lazy.
- **Concurrent compilation of *different* shapes is a data race.** Found by
  `-race`, not by reading: the graph function reaches
  `gomlx/ml/zoo/transformer.populateOrderedScopes`, which lazily fills a
  `*kvcache.KVCache` shared by every graph built from the same model. GoMLX
  singleflights per *shape*, which does not help here. `localembed` serializes
  compilation behind `compileMu` and records compiled shapes itself; execution
  stays fully concurrent. Steady-state cost is nil — each of the 21 shapes is
  compiled once.

### No memory leak

A first reading of `runtime.Sys` showed +333 MiB over 40 batches and looked
alarming. `Sys` is reserved-not-returned and monotonic by design, so it reports
growth even when nothing leaks. Measuring `HeapAlloc` plus real RSS instead:
the `go` backend's heap returns to its exact baseline after `runtime.GC()`, and
`xla` stays flat because weights live in PJRT device buffers rather than the Go
heap.

Confirmed against this implementation by `TestSoak_NoLeak` (200 batches across
every shape in the closed set): after GC the heap settles at **30.3 MiB whether
the soak runs 4 rounds or 10**. The ~21 MiB above the 9 MiB baseline is the 20
compiled graphs — bounded by the closed shape set, not growing. RSS plateaus
around 1.1 GiB on XLA.

### API corrections

The original plan named several things that do not exist:

| Plan said | Actually |
|---|---|
| `PreCompile(...)` | `Exec.Compile(inputShapes ...shapes.Shape)` |
| `tensors.CopyFlatData` | does not exist — copy inside the `Tensor.ConstFlatData(func(any))` callback |
| `FinalizeAll()` free function | `Tensor.FinalizeAll() error` method |
| tokens as `[]int64` | `int32` |
| registry must hardcode dims | `hfModel.Config.HiddenSize` reports them |

## Design

### Shape bucketing

GoMLX compiles one executable per distinct `[batch, seqLen]` input shape, so both
axes are quantized to a closed set — 3 batch buckets × 7 seq buckets = 21 shapes
at the defaults:

- seq → powers of two `8, 16, 32 … maxSeqLen`
- batch → `1, 8, batchSize`
- a short final batch is **padded up to the next bucket**, surplus rows
  discarded. Without this, every index run's tail produces a one-off shape, which
  is the most likely way the graph cache grows without bound.

`bucketize` is a pure function rather than go-huggingface's streaming
`bucket.Run`, so the closed-set invariant can be asserted directly in a test and
there is no channel close/drain/cancel lifecycle to deadlock on.

### Deadlock safety

The design removes the deadlock classes rather than guarding them.

- **No mutex around inference.** Concurrency is capped by one
  `semaphore.Weighted`, acquired with `ctx` so cancellation always breaks the
  wait, released in a `defer`, with exactly one acquisition point.
- **No channels in the hot path.** `bucketize` is pure.
- **Fan-out writes disjoint index ranges**, so there is no shared mutable state
  and no lock. `bucketize` guarantees the index sets are disjoint; a test asserts
  it, including that the slices do not alias.
- **Tokenization is a separate phase**, before any semaphore slot is taken, so
  `tokMu` is never held while acquiring `sem` and cannot form a cycle.
- **`Close` does not drain through the semaphore.** This is a deliberate
  departure from the original plan, which proposed acquiring every slot. That
  deadlocks: a caller that has already passed the closed check then blocks
  forever on `Acquire`, because `Close` never releases. Instead an `RWMutex`
  makes "not closed" and "registered in-flight" a single atomic step, and `Close`
  waits on a `WaitGroup` with a bounded timeout.
- **Small batches, not many goroutines.** GoMLX parallelizes internally, so
  fanning out across `IndexConcurrency` (default 20) would oversubscribe cores.
  In-flight batches are capped at 2 — the inverse of what `openai.Client` does,
  so it carries an explicit comment.

### Memory safety

GoMLX buffers are freed explicitly, not by the GC; under XLA they are device
buffers whose ceiling is VRAM.

- Result `float32`s are copied **inside** the `ConstFlatData` callback (there is
  no `CopyFlatData`), and both input and output tensors are finalized in `defer`s
  in the same function. A tensor never escapes the call that created it.
- `Close` finalizes in reverse construction order — `Exec`, store, `Backend` —
  under a `sync.Once`.
- If the drain times out, `Close` returns `ErrShutdownTimeout` and **skips
  finalization**. Freeing buffers under a live call is undefined behaviour;
  leaking them until process exit is harmless. The asymmetry is intentional.
- One model per process: `WithPrefix` returns a view sharing the weights, the
  backend, the graph cache, **and the semaphore**. Sharing the semaphore is
  required, not incidental — it is what makes the owner's `Close` drain cover
  work issued through the view. Verified end to end: `codamigo serve` builds two
  embedders and logs exactly one backend construction.
- **A view is not a substitute for the owner.** `WithPrefix` sets `owner: false`,
  so its `Close` is a no-op. A caller that needs only the query side must
  therefore pass `Options.ApplyQueryPrefix` and receive the owner with the prefix
  already applied — the first cut of `newLocalEmbedder` returned
  `emb.WithPrefix(emb.QueryPrefix())` and dropped the owner on the floor, which
  made `defer closeEmbedder` inert for `search`, `map`, `callers`, `doctor`, and
  `init`: the backend was never finalized. `WithPrefix` survives for the one case
  it is right for, `queryEmbedderFor`, which derives the query side from a
  document embedder the caller already owns. Guarded by
  `TestApplyQueryPrefix_OwnsClose`.

### Supply-chain pinning

Registry models pin a revision **and** a per-file SHA256. go-huggingface caches
by ETag and never compares a content hash, and HuggingFace publishes real SHA256
only for LFS files — not for small ones like `tokenizer.json`. Comparing our own
constants against a pinned revision is what makes the download reproducible
rather than merely uncorrupted.

A file that fails verification is deleted (along with the blob it links to)
before the error returns, so a retry starts clean.

The PJRT compute plugin is **not** verified this way: go-xla downloads GitHub
release assets with an empty hash and a `// TODO` upstream. `download-model`
says so rather than implying otherwise.

### Registry contents

| Model | Dims | Size | Status |
|---|---:|---:|---|
| `bge-small-en-v1.5` (default) | 384 | 133 MB | Pinned, verified, asymmetric (query prefix) |
| `all-MiniLM-L6-v2` | 384 | 91 MB | Pinned, verified, symmetric |

`nomic-embed-text-v1.5` was in the original plan and is **deliberately absent**:
`transformer.LoadModel` fails on it with `cannot unmarshal number into Go struct
field wrapper.rope_parameters of type transformer.RoPEParams`. Listing it would
promise more than the loader delivers. This was found by probing, not assumed.

Every registry entry is asserted by test to have a 40-character revision and a
64-character SHA256 per file, because an entry with an invented checksum is worse
than no entry — `Download` would reject a legitimate file.

## Defects fixed along the way

1. **`doctor` opened the store with the wrong dimension.** It passed
   `cfg.EmbeddingDimensions`, while index/search/map/graph/serve all pass
   `emb.Dim()`. Since `Defaults()` sets 1536, `embedding_provider: local` with a
   384-dim model produced a spurious `[FAIL] Store open error`. Fixed by building
   the embedder before the store section and introducing `storeDim`. Verified by
   running `doctor` with `embedding_dimensions` unset against a 384-dim model.
2. **`init` bypassed the embedder factory**, constructing `openai.New` inline, so
   the local provider would never have been covered by its smoke test. Now routed
   through `newEmbedder`.
3. **`store.validateSchema` named a manual fix** ("delete the DB file and
   re-run") for all three mismatch messages. Now names `codamigo reset`.
4. **The query-side local embedder was never closed.** `newLocalEmbedder`
   returned `emb.WithPrefix(emb.QueryPrefix())` for `roleQuery` and dropped the
   owner, so the `defer closeEmbedder` at all five query-side call sites
   (`search`, `map`, `callers`/`callees`/`impact`, `doctor`, `init`) was a no-op
   and the compute backend, graph cache, and weights were never finalized —
   exactly the invariant AGENTS.md claims the `io.Closer` machinery enforces.
   Fixed with `Options.ApplyQueryPrefix`, which returns the owner with the prefix
   applied. Guarded by `TestApplyQueryPrefix_OwnsClose`, confirmed to fail when
   the old behaviour is reintroduced.

### Correcting the throughput table

The spike's `xla` column did not survive being turned into a repeatable
benchmark, and the first version of `BenchmarkEmbedBatch` was wrong in two ways
worth recording:

1. **The seq labels were off by one bucket.** `strings.Repeat("token ", seqLen)`
   yields `seqLen` tokens *plus* CLS and SEP, so every row ran in the next bucket
   up. The tell was `go/seq256` and `go/seq512` measuring 0.49 and 0.46 emb/s —
   near-identical because both landed in the 512 bucket. Fixed with `seqLen-2`
   repetitions.
2. **It amortized graph compilation into the throughput figure.** Compilation is
   once per shape per process, so including it understated `xla` by roughly half.
   Fixed with a warm-up call before `b.Loop()`.

With both fixed, the `go` column reproduces the spike almost exactly
(7.5/3.5/1.5/0.5 vs 7.7/3.5/1.6/0.5), which is what gives confidence in the
method. The `xla` column does not: 186.6 vs 94.7 at seq 32. The spike's number
included per-call overhead that a steady-state loop does not, and seq 32 — at
43ms for 8 embeddings — is where that overhead dominates. The published range
moved from "12–17×" to **12–25×** accordingly. Every copy of the old figures
(README twice, `backend.go`, `download_cmd.go`, `backend_test.go`) was updated;
that many copies of one measurement is itself an argument for the benchmark.

## Verified

`make fmt && make vet && make test && make build`, plus end to end on a copy of
this repository with the local provider:

- `download-model` with `HF_TOKEN` and `CODAMIGO_HF_TOKEN` unset: 7 files,
  128.0 MiB, all verified against the pinned checksums. A second run downloads
  nothing.
- `index`: 32s for 427 chunks across 32 files, backend `xla`, no network.
- `search "sequence length bucketing"` returns `localembed/bucket.go` first.
- `callers bucketize` returns 5 callers — this exercises the `graph_cmd.go` call
  site the original plan missed.
- `doctor` reports provider, model, files present, token limit, backend `xla`,
  and index stats with **no spurious store dimension failure**.
- `serve` answers an MCP `tools/call` `search` request, with one backend
  construction for two embedders.
- Golden vectors match the Python reference at cosine ≥ 0.999 on **both** `go`
  and `xla`, including the padded-batch case.
- `-race` clean across all packages.
- `BenchmarkEmbedBatch` reproduces the throughput table above, and
  `TestApplyQueryPrefix_OwnsClose` was confirmed to fail against the defect it
  guards.
- `make docker` builds, and `docker run -v ~/.codamigo/models:/root/.codamigo/models
  codamigo:cpu doctor` finds the mounted weights and embeds at 384 dims. It
  correctly reports `Compute backend: go`, since `GOMLX_NO_AUTO_INSTALL=1` is set
  and no plugin is baked in.

  This did **not** work as first written. `sqlite-vec`'s C sources include
  `sqlite3.h`, which the `golang:1.26.5-bookworm` image does not ship, so the
  builder stage failed with `sqlite-vec.h:7:10: fatal error: sqlite3.h: No such
  file or directory`. Easy to miss from macOS, where the SDK provides the header —
  the Dockerfile now installs `libsqlite3-dev`, and `Dockerfile.cuda` inherits the
  same fix.

## Not verified

**`Dockerfile.cuda` and the `xla:cuda` backend have not been built or run.** Both
need a linux/amd64 host with an NVIDIA GPU; this was developed on macOS/arm64,
where Docker correctly reports no CUDA device. The `installer.CudaInstall` call
signature was type-checked for linux/amd64 (and confirmed absent on darwin, which
is why `cmd/codamigo/cuda_*.go` is split by build constraint), but the image
build, the cuDNN package name, and the baked plugin path are unconfirmed. Treat
them as the first things to check.

Live round-trip against a remote provider was not re-tested — no API key was
available in the development environment. The dispatch is covered by unit tests
asserting that `openai`, `voyage`, and unknown provider names all build the HTTP
client.

## Rejected

- **A provider allow-list in `config.Validate`.** Would break `voyage`.
- **Baking model weights into the Docker images.** 133 MB, and which model to use
  is a configuration decision. Mount `~/.codamigo/models` instead.
- **`CGO_ENABLED=0` static builds.** Impossible: `mattn/go-sqlite3` and the
  tree-sitter grammars are both cgo.
- **`depguard` / `.golangci.yml` to enforce layering.** No such file exists and
  `golangci-lint` is not installed here, so `make lint` is not a gate.
  `internal/arch` is the mechanism that actually runs.
- **Flattening the go-huggingface cache layout.** Would mean giving up
  `transformer.LoadModel` (which takes a `*hub.Repo`) and hand-rolling
  safetensors loading. Deleting one model is still `rm -rf` of one directory.
