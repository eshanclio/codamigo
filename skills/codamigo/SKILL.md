---
name: codamigo
description: >
  Use when exploring, searching, or understanding a codebase — finding symbols,
  locating implementations, understanding structure, or fetching relevant code.
  Also use when the user says "find", "where is", "how does X work", "look up",
  "which file", "show me the code for", "search codebase", or "codamigo".
argument-hint: "[query or concept]"
allowed-tools:
  - Bash
  - Read
---

# codamigo — Token-Efficient Codebase Search

Semantic search over source code via tree-sitter AST chunking and hybrid KNN + BM25 retrieval.

## MCP or CLI

If the `mcp__codamigo__*` tools are in your tool list, use them directly — same parameters,
no process spawn. Fall back to CLI only if MCP tools are unavailable.

| MCP tool | CLI equivalent |
|----------|----------------|
| `mcp__codamigo__search` | `codamigo search` |
| `mcp__codamigo__get_map` | `codamigo map` |
| `mcp__codamigo__get_callers` | `codamigo callers` |
| `mcp__codamigo__get_callees` | `codamigo callees` |
| `mcp__codamigo__get_impact` | `codamigo impact` |

## Health Check (once per session)

```bash
codamigo doctor --quick
```

| Outcome | Action |
|---------|--------|
| Index healthy | Proceed |
| Index missing/empty | Tell user: "Run `codamigo index` to build (requires embedding API key)." Stop. |
| Binary not found | Tell user codamigo is not installed. Stop. |

**Never run `codamigo index` without user confirmation** — it calls a paid embedding API.

## Workflow: Orient → Locate → Fetch

Skip phases when context is already available. Jump to Fetch if the exact symbol name is known.

Queries are semantic — describe the concept ("parse config file"), not the syntax ("func Load").

### Phase 1: Orient (free, no API call)

```bash
codamigo map --max-tokens 2000
```

Returns packages, files, and symbol names. By default:
- Non-code files are excluded (configurable via `non_code_languages` in settings)
- Symbols show line ranges (e.g. `func NewServer:10-25`)
- Exported symbols are marked with `+`, internal with `-`

Use `--no-code-only` to include non-code files when needed. Use to identify which package or path to scope into.

### Phase 2: Locate (~80% token savings)

```bash
codamigo search "<concept>" --metadata-only --limit 20
```

Output: `filepath:line  name  node_kind` — one line per result, no source content.

Narrow with filters:

```bash
codamigo search "<concept>" --metadata-only --package <pkg>
codamigo search "<concept>" --metadata-only --lang go
codamigo search "<concept>" --metadata-only --node-kind function_declaration
```

Use `--node-kind` when looking for a specific symbol type (functions, types, classes).
Use `--offset 20` to paginate if the first page doesn't contain what you need.

### Phase 3: Fetch

```bash
# exact symbol — cheapest
codamigo search "<symbol>" --name <exact_name> --limit 3

# broader — always set a token budget
codamigo search "<concept>" --package <pkg> --limit 5 --max-tokens 4000
```

Output: `score  filepath:startLine-endLine [language]` header followed by chunk source code.

Set `--max-tokens` to cap total output. A `(truncated to token budget)` trailer signals more exist.

**Freshness (MCP only).** `mcp__codamigo__search` reconciles results against the files on disk: a
few changed files are re-indexed in place, and beyond that the affected results come back marked
`"stale": true` (or `(stale — read file to confirm)` in metadata mode). Treat a stale result as a
pointer, not as content — read the file at that line. The `codamigo search` CLI does not reconcile,
so its snippets can lag the working tree.

### Phase 4: Traverse

Once you have a symbol name, follow its relationships instead of grepping for it. These read the
prebuilt code graph — **no embedding calls, so they are free** and far cheaper than a text search
that returns every mention of a common name.

```bash
# who depends on this? — before changing or deleting it
codamigo callers <symbol>

# what does this use? — to trace a call chain outward
codamigo callees <symbol>

# blast radius — before renaming or changing a signature
codamigo impact <symbol> --depth 2
```

Output: `filepath:line symbol`, one line per result.

- `callers` and `impact` point at the **reference site** — the line to open.
- `callees` points at the **definition** being referenced.
- `(external)` marks a target with no definition in this project (a third-party or stdlib symbol).
- `(reference)` / `(inherit)` mark a relationship that is not a plain call.
- `impact` reports `depth=N`, the number of hops from the symbol you asked about.

Use this when the question is "what breaks if I change X" or "where is X actually used" —
`search` answers "what code is about X", which is a different question.

If a graph command reports that the graph is not built, the index was built with `enable_graph:
false`. Ask the user before running `codamigo index` to rebuild it.

For all flags see [references/commands.md](references/commands.md).
For configuration see [references/settings.md](references/settings.md).

## Guard Rails

1. **Never fetch full source on the first search.** Use `--metadata-only` first unless the exact symbol name is known.
2. **Never run `codamigo index` without user confirmation.**
3. **Always set `--max-tokens` when fetching more than 3 results.**
4. **Stop after 3 unsuccessful rounds.** Ask the user to clarify.
5. **Prefer filters over high limits.** `--package`, `--lang`, `--node-kind` are cheaper than raising `--limit`.
6. **Paginate before reformulating.** Try `--offset` before rewriting the query.
7. **Traverse instead of grepping for usages.** `callers` costs nothing and is exact; a text
   search for a common name burns tokens on unrelated matches.
