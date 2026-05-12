# codamigo Command Reference

## search

```bash
codamigo search "<query>" [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--metadata-only` | bool | false | Return `filepath:line  name  node_kind` only; no source content |
| `--limit` | int | 10 | Max results (clamped 1–100) |
| `--offset` | int | 0 | Skip N results for pagination |
| `--max-tokens` | int | 0 | Token budget for output; 0 = no limit |
| `--package` | string | — | Filter to package (e.g. `store`, `cmd/codamigo`) |
| `--lang` | string | — | Filter by language, repeatable (`--lang go --lang python`) |
| `--path` | string | — | Glob filter on file path (`--path 'cmd/**'`) |
| `--name` | string | — | Exact symbol name match (e.g. `NewChunker`) |
| `--node-kind` | string | — | Filter by AST node kind, repeatable |

Common node kinds: `function_declaration`, `method_declaration`, `type_declaration`,
`function_definition`, `class_definition`, `class_declaration`.

## map

```bash
codamigo map [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--max-tokens` | int | 2000 | Token budget for output |

Returns a symbol tree grouped by package. No API call — reads from index only.

## doctor

```bash
codamigo doctor [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--quick` | bool | false | Skip live embedding smoke-test (no network call) |

Reports: config paths, store existence, index stats, embedding API connectivity.

## reset

```bash
codamigo reset [--force]
```

Deletes the store database. Requires `codamigo index` to rebuild.
