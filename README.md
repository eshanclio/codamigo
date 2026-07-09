# Codamigo - Code Amigo

[![Go Reference](https://pkg.go.dev/badge/github.com/ieshan/codamigo.svg)](https://pkg.go.dev/github.com/ieshan/codamigo)
[![Go Report Card](https://goreportcard.com/badge/github.com/ieshan/codamigo)](https://goreportcard.com/report/github.com/ieshan/codamigo)

Semantic code search for your local machine. codamigo walks a source tree, chunks files into semantically coherent pieces using tree-sitter ASTs, embeds the chunks with a configurable embedding provider, stores them in a local SQLite + sqlite-vec database, and exposes hybrid search (KNN + BM25) via a CLI and an MCP stdio server.

## How it works

codamigo runs a four-stage pipeline:

- **Walk** — recursive filesystem walk with `.gitignore` / `.caignore` and include/exclude glob filtering
- **Chunk** — tree-sitter AST-based splitting into semantically coherent units (functions, classes, declarations)
- **Embed** — each chunk is converted to a float32 vector via any OpenAI-compatible embedding API
- **Search** — queries are embedded and matched against the store using hybrid KNN + BM25 (Reciprocal Rank Fusion), backed by sqlite-vec and FTS5

The index lives in a single `.codamigo/store.db` file in your project. No external services are required — any local embedding server (Ollama, LM Studio) works.

## Installation

Prerequisites: Go 1.26+ and a C compiler (CGo is required for tree-sitter grammars).

```sh
go install github.com/ieshan/codamigo/cmd/codamigo@latest
```

If `cc` is not on your PATH, set the `CC` environment variable to point to your compiler before building.

## Quick start

```sh
codamigo init            # guided setup — writes ~/.codamigo/global_settings.yml
codamigo index           # walk + chunk + embed + store
codamigo search "query"  # semantic search, prints top 10 results
codamigo map             # structural map of packages, files, and symbols
codamigo serve           # start MCP stdio server for AI assistant integration
```

## Usage modes

Choose the mode that fits your workflow.

### Mode A — CLI only

Run `index` whenever you want to refresh the store, then `search` interactively. No AI assistant required. Useful for one-off exploration or scripting.

```sh
codamigo index
codamigo search "authentication middleware" 5
```

### Mode B — MCP with pre-indexed store

Pre-run `index` before starting `serve` (e.g. in CI, as a cron job, or on a git hook). When `serve` starts it also runs an initial index pass, but if the store is already fresh all files are hash-matched and skipped quickly. Use this when you want indexing managed outside the MCP server lifecycle.

```sh
# scheduled or on commit:
codamigo index

# AI assistant launches this (serve re-checks hashes on startup, then watches):
codamigo serve
```

### Mode C — MCP with auto-indexing (recommended)

Run `serve` once. It performs an initial full index on startup, then watches the filesystem for changes and re-indexes modified files continuously. The MCP `refresh_index=true` parameter triggers a manual re-index on demand.

```sh
# configure your AI assistant to run:
codamigo serve
```

**Decision guide:**
- Simplest local setup → **Mode C**
- Indexing managed separately (CI/CD, cron, git hooks) → **Mode B**
- No AI assistant, just want semantic grep → **Mode A**

## Commands

### `codamigo init`

Guided first-time setup. Prompts for embedding base URL, model, and API key. Writes `~/.codamigo/global_settings.yml` and `.codamigo/settings.yml` (if absent). Appends `.codamigo/` to `.gitignore`. Runs a smoke-test against the embedding model.

No flags. Reads from stdin interactively.

---

### `codamigo index`

Walks the project root, chunks every matched file, embeds each chunk, and upserts records into the store. Skips files whose content hash is unchanged since the last run. Removes records for files that no longer exist on disk.

When stderr is a TTY, a live 2-line progress display shows running processed/skipped counts and the file currently being indexed. The display is suppressed automatically in CI and piped environments.

**Common flags (shared by all commands):**

| Flag | Env var | Default | Purpose |
|------|---------|---------|---------|
| `--api-key` | `CODAMIGO_API_KEY` | — | Embedding API key |
| `--model` | `CODAMIGO_MODEL` | `text-embedding-3-small` | Embedding model name |
| `--base-url` | `CODAMIGO_BASE_URL` | `https://api.openai.com/v1` | Embedding API base URL |
| `--store-path` | `CODAMIGO_STORE_PATH` | `.codamigo/store.db` | SQLite store file path |
| `--project-root` | `CODAMIGO_PROJECT_ROOT` | current directory | Root directory to index |
| `--dimensions` | `CODAMIGO_DIMENSIONS` | `1536` | Embedding vector dimensions |
| `--global-config` | `CODAMIGO_GLOBAL_CONFIG` | `~/.codamigo/global_settings.yml` | Path to global config |
| `--project-config` | `CODAMIGO_PROJECT_CONFIG` | `.codamigo/settings.yml` | Path to project config |

---

### `codamigo search <query> [limit]`

Embeds the query and runs hybrid KNN + BM25 search. Prints results as `score filepath:startLine-endLine [language]` followed by the chunk content.

**Additional flags:**

| Flag | Default | Purpose |
|------|---------|---------|
| `--limit` | `10` | Maximum results to return |
| `--offset` | `0` | Results to skip (pagination) |
| `--lang` | — | Filter by language, repeatable: `--lang go --lang python` |
| `--path` | — | Filter by file path glob, repeatable: `--path 'cmd/**'` |
| `--max-tokens` | `0` | Token budget for results (0 = no limit) |
| `--package` | — | Filter results to a package (e.g. `--package store`) |
| `--name` | — | Filter by symbol name |
| `--node-kind` | — | Filter by AST node kind, repeatable |
| `--metadata-only` | `false` | Return only file paths, line numbers, and symbol names |

`limit` can also be passed as a second positional argument: `codamigo search "auth" 5`.

---

### `codamigo map`

Prints a structural map of the indexed codebase showing packages, files, and symbol names. Useful for orientation before searching. Built entirely from stored data — no embedding API calls.

By default, the map excludes configured non-code files (default: markdown, yaml, json), shows line ranges on symbols, includes per-file type summaries, and marks exported/internal symbols.

| Flag | Default | Purpose |
|------|---------|---------|
| `--max-tokens` | `2000` | Token budget for the map output |
| `--no-code-only` | `false` | Include configured non-code language files in the map |
| `--no-summary` | `false` | Hide per-file type summary from file headers |
| `--no-visibility` | `false` | Hide export/visibility markers from symbols |

---

### `codamigo serve`

Starts the MCP stdio server. On startup it runs a full index pass, then launches a background filesystem watcher that re-indexes changed files. Accepts MCP tool calls from an AI assistant over stdin/stdout.

**MCP tool: `search`**
- `query` (string) — the search text
- `limit` (int, default `10`) — how many results to return
- `languages` (array, optional) — filter by programming language
- `paths` (array, optional) — glob patterns to restrict search scope
- `max_tokens` (int, default `0`) — token budget for results (0 = no limit)
- `package` (string, optional) — filter to a package name
- `refresh_index` (bool, default `false`) — trigger a full re-index before searching
- `name` (string, optional) — filter results to chunks matching this symbol name
- `node_kinds` (array, optional) — filter by AST node kind (e.g. `["function_declaration"]`)
- `metadata_only` (bool, default `false`) — return only file paths, line numbers, and symbol names (no source content)
- `offset` (int, default `0`) — number of results to skip for pagination

**MCP tool: `get_map`**
- `max_tokens` (int, default `2000`) — token budget for the map output
- `code_only` (bool, default `true`) — exclude configured non-code languages from the map
- `show_summary` (bool, default `true`) — show per-file type summary in file headers
- `show_visibility` (bool, default `true`) — show export markers (`+` public, `-` internal)

Uses the same common flags as `index`.

---

### `codamigo reset`

Deletes the vector store database file. Prompts for confirmation unless `--force` is passed.

| Flag | Purpose |
|------|---------|
| `--force` | Skip the confirmation prompt |

---

### `codamigo doctor`

Diagnoses configuration, store health, and embedding model reachability. Reports: global config, project config, store file existence, index stats (chunks, files, per-language counts), walker file count, embedding smoke-test.

| Flag | Purpose |
|------|---------|
| `--quick` | Skip the live embedding smoke-test |

---

## Configuration

Configuration is loaded in four layers — later layers win:

```
built-in defaults
  → ~/.codamigo/global_settings.yml   (shared across all projects)
    → .codamigo/settings.yml          (per-project; safe to commit)
      → environment variables
        → CLI flags
```

`codamigo init` writes the global file. The project file holds project-specific patterns. Both are YAML.

**Full config reference:**

```yaml
# Embedding provider
embedding_provider: openai            # informational label only
embedding_model: text-embedding-3-small
embedding_api_key: sk-...             # use CODAMIGO_API_KEY env var instead
embedding_base_url: https://api.openai.com/v1
embedding_dimensions: 1536
embedding_index_input_type: ""        # e.g. "document" for Voyage AI
embedding_query_input_type: ""        # e.g. "query" for Voyage AI

# Rate limiting and retries
embedding_max_batch_size: 256
embedding_rate_limit: 500.0           # sustained requests/second
embedding_rate_burst: 100             # max burst above sustained rate
embedding_max_retries: 3
embedding_retry_base_delay: "500ms"   # e.g. "500ms", "1s"
embedding_http_timeout: "60s"         # per-request timeout; increase for slow local backends

# File filtering
include_patterns: []                  # empty = include all matched extensions
exclude_patterns: []                  # gitignore rules are also applied

# Map display
non_code_languages:           # languages excluded by code_only filter
  - markdown                  # default: ["markdown", "yaml", "json"]
  - yaml
  - json

# Storage
store_path: .codamigo/store.db

# Project
project_root: ""                      # defaults to current working directory

# Indexing
index_concurrency: 20                 # files processed concurrently during indexing
max_file_size: 1048576                # skip files larger than this (bytes); 0 = no limit
write_batch_size: 50                  # files per DB write transaction during batch indexing; 0 = use default (50)

# File watching (serve only)
watch_mode: auto                      # "auto" | "fsnotify" | "poll"
poll_interval: "5s"
debounce_window: "500ms"
```

Keep `embedding_api_key` in the global config (written with mode `0600` by `init`) or in `CODAMIGO_API_KEY`. Do not put API keys in the project config.

### Docker

| Environment | Recommendation |
|-------------|----------------|
| Docker on Linux | `watch_mode: auto` works — inotify in the container shares the host kernel; auto-fallback handles watch limits |
| Docker on macOS | Set `watch_mode: poll` — Docker Desktop's osxfs does not reliably propagate host-side filesystem changes into containers as kernel events |
| Docker on Windows (WSL2) | Set `watch_mode: poll` — same bind-mount event propagation limitation as macOS |

For Docker environments using polling, `poll_interval: "2s"` is a good default to reduce overhead:

```yaml
watch_mode: poll
poll_interval: "2s"
```

## Embedding providers

### OpenAI

```yaml
# ~/.codamigo/global_settings.yml
embedding_base_url: https://api.openai.com/v1
embedding_model: text-embedding-3-small
embedding_dimensions: 1536
```

```sh
export CODAMIGO_API_KEY=sk-...
```

Models: `text-embedding-3-small` (fast, 1536 dims), `text-embedding-3-large` (higher quality, 3072 dims).

---

### Voyage AI

Voyage uses `input_type` to distinguish document vs. query vectors, which improves retrieval quality.

```yaml
# ~/.codamigo/global_settings.yml
embedding_base_url: https://api.voyageai.com/v1
embedding_model: voyage-code-3
embedding_dimensions: 1024
embedding_index_input_type: document
embedding_query_input_type: query
```

```sh
export CODAMIGO_API_KEY=pa-...
```

---

### Ollama (local)

No API key required. Pull a model first:

```sh
ollama pull nomic-embed-text
```

```yaml
# ~/.codamigo/global_settings.yml
embedding_base_url: http://localhost:11434/v1
embedding_model: nomic-embed-text
embedding_dimensions: 768
embedding_rate_limit: 50
embedding_rate_burst: 10
```

Ollama requires a non-empty `Authorization` header; set any placeholder value:

```sh
export CODAMIGO_API_KEY=ollama
```

Good models: `nomic-embed-text` (768 dims), `mxbai-embed-large` (1024 dims).

---

### LM Studio (local)

Enable **Local Server** in LM Studio and load an embedding model, then:

```yaml
# ~/.codamigo/global_settings.yml
embedding_base_url: http://localhost:1234/v1
embedding_model: <model-id-shown-in-lm-studio>
embedding_dimensions: <see model card>
embedding_rate_limit: 20
embedding_rate_burst: 5
```

```sh
export CODAMIGO_API_KEY=lm-studio
```

Check the model card for `embedding_dimensions`. A mismatch between the stored value and the configured value causes an error on the second `index` run — the store enforces model consistency.

## MCP integration

codamigo speaks the MCP stdio protocol. Configure your AI assistant to launch `codamigo serve` as a stdio MCP server. The server indexes on startup and keeps the index fresh via filesystem watching.

### Claude Code

Add to `~/.claude/settings.json` (global) or `.claude/settings.json` (project):

```json
{
  "mcpServers": {
    "codamigo": {
      "command": "codamigo",
      "args": ["serve"],
      "env": {
        "CODAMIGO_API_KEY": "<your-api-key>"
      }
    }
  }
}
```

If your API key is already in `~/.codamigo/global_settings.yml`, the `env` block can be omitted.

The tools are available in Claude as `mcp__codamigo__search` and `mcp__codamigo__get_map`.

### OpenAI Codex

Add to `~/.codex/config.toml` (global) or `codex.toml` (project):

```toml
[[mcp_servers]]
name = "codamigo"
command = "codamigo"
args = ["serve"]

[mcp_servers.env]
CODAMIGO_API_KEY = "<your-api-key>"
```

**Tip:** For large repos, run `codamigo index` once before starting your AI session. When `serve` starts it re-checks all files, but if the store is already fresh the pass completes in seconds.

## Using with AI coding agents

codamigo is designed to be used as an MCP server by AI coding agents such as
Claude Code, OpenAI Codex, Cursor, Windsurf, and others. Once `codamigo serve`
is running, the agent has access to two tools:

| Tool | Purpose |
|------|---------|
| `search` | Semantic search — embed a query and return matching code chunks |
| `get_map` | Structural overview — packages, files, and symbol names from the index |

### Recommended workflow

**1. Orient with `get_map` first.**
Before searching, call `get_map` (with a reasonable `max_tokens` budget, e.g.
`2000`) to get a structural overview of the codebase. This shows which packages
exist, how many symbols each contains, and what the key files are. Use this to
decide which package or file to scope your search to.

**2. Search semantically.**
Use natural-language queries rather than exact symbol names. The hybrid KNN +
BM25 index understands intent, not just keywords. "parse config file" will find
the config loading logic even if the function is called `Load`.

**3. Scope searches to reduce noise.**
Narrow results with the available filters:
- `package` — restrict to one package, e.g. `"store"` or `"cmd/codamigo"`
- `languages` — e.g. `["go"]` to skip test fixtures in other languages
- `node_kinds` — e.g. `["function_declaration", "method_declaration"]` to see only functions
- `name` — exact symbol lookup, e.g. `"NewChunker"`

**4. Use `metadata_only` for exploratory queries.**
When you want to find which files or functions are relevant without reading their
full source, set `metadata_only=true`. Results include file path, line numbers,
and symbol name but omit the source text — typically 10–20× fewer tokens.
Follow up with a targeted search (or a direct file read) once you've identified
the right symbols.

**5. Control context budget with `max_tokens`.**
For agents with limited context windows, set `max_tokens` to cap the total
tokens returned. Results are ranked by relevance and truncated at the budget;
a `truncated: true` flag signals that more results exist.

**6. Refresh the index when needed.**
Set `refresh_index=true` on a search call to trigger a full re-index before
querying. A 30-second cooldown prevents hammering the embedder on rapid
back-to-back calls. Alternatively, run `codamigo index` from the shell.

### Example agent queries (Claude Code)

```
# Overview first — all features enabled by default
mcp__codamigo__get_map(max_tokens=3000)

# Overview without visibility markers
mcp__codamigo__get_map(max_tokens=3000, show_visibility=false)

# Include non-code files (markdown, yaml, etc.)
mcp__codamigo__get_map(max_tokens=3000, code_only=false)

# Find all functions related to embedding
mcp__codamigo__search(query="embedding API request", package="cmd/codamigo", node_kinds=["function_declaration"])

# Look up a specific symbol
mcp__codamigo__search(query="walk directory tree", name="Walk", metadata_only=true)

# Scan store package cheaply
mcp__codamigo__search(query="upsert chunk records", package="store", metadata_only=true, limit=20)
```

### Node kind reference

Common values for the `node_kinds` filter:

| Value | Matches |
|-------|---------|
| `function_declaration` | Go `func` at package level |
| `method_declaration` | Go `func` on a receiver |
| `type_declaration` | Go `type` block |
| `function_definition` | Python / C / C++ functions |
| `class_definition` | Python classes |
| `class_declaration` | TypeScript / Java classes |
| `method_definition` | JS / TS / Ruby methods |

Run `codamigo map` to see which node kinds appear in your indexed codebase.

---

## Supported languages

| Language | Extensions |
|----------|------------|
| Go | `.go` |
| Python | `.py`, `.pyw` |
| JavaScript | `.js`, `.mjs`, `.cjs`, `.jsx` |
| TypeScript | `.ts`, `.mts` |
| TSX | `.tsx` |
| Ruby | `.rb` |
| C | `.c`, `.h` |
| C++ | `.cpp`, `.cc`, `.cxx`, `.hpp` |
| Bash | `.sh`, `.bash` |
| HTML | `.html`, `.htm` |
| CSS | `.css` |
| Markdown | `.md`, `.markdown` |
| JSON | `.json` |
| YAML | `.yaml`, `.yml` |
| Vue | `.vue` |

Use `include_patterns` and `exclude_patterns` in your project config to control which files are indexed.

## `.caignore`

codamigo supports a `.caignore` file that works exactly like `.gitignore` but is specific to codamigo. Files matched by either `.gitignore` or `.caignore` are excluded from indexing and file watching.

### Why use `.caignore`?

Your `.gitignore` controls what Git tracks. Sometimes you want codamigo to skip files that Git still tracks — large generated files, vendored dependencies, test fixtures, or data files that add noise to search results. `.caignore` lets you tune codamigo's scope without touching `.gitignore`.

### Syntax

`.caignore` uses identical syntax to `.gitignore`:

```
# Ignore all CSV data files
*.csv

# Ignore the testdata directory
testdata/

# But keep the golden files
!testdata/golden/
```

### Behavior

- **Same directory scoping as `.gitignore`.** A `.caignore` in `src/` applies only to paths under `src/`, just like a nested `.gitignore`.
- **`.caignore` rules win on conflict.** Both files are loaded per directory (`.gitignore` first, then `.caignore`). The "last matching rule wins" semantics mean `.caignore` takes precedence.
- **Negation works across files.** A `!pattern` in `.caignore` can re-include a path that `.gitignore` excludes.
- **Either file is optional.** A directory with only `.caignore` (no `.gitignore`) works. A directory with only `.gitignore` works as before.

### Examples

Exclude large generated files from the index while keeping them in Git:

```
# .caignore
generated/
*.pb.go
*.min.js
```

Re-include a directory that `.gitignore` excludes (useful for vendored code you want searchable):

```
# .gitignore
vendor/

# .caignore — override .gitignore for codamigo
!vendor/
```

Scope exclusions to a subdirectory by placing `.caignore` there:

```
# frontend/.caignore — only affects frontend/
node_modules/
dist/
*.bundle.js
```
