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

## callers

```bash
codamigo callers <symbol> [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--limit` | int | 50 | Maximum number of results to print |

Lists the definitions that reference `<symbol>`. No API call — reads the code graph only.
Each line points at the **reference site**, the line to open. A definition that references the
symbol several times is reported once.

## callees

```bash
codamigo callees <symbol> [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--limit` | int | 50 | Maximum number of results to print |

Lists what `<symbol>` references: functions it calls, types it names, supertypes it declares.
Each line points at the **definition** being referenced. Targets with no definition in the project
are marked `(external)`. Unresolved type references (builtins such as `string` and `error`) are
omitted; unresolved calls are kept.

## impact

```bash
codamigo impact <symbol> [flags]
```

| Flag | Type | Default | Description |
|------|------|---------|-------------|
| `--depth` | int | 2 | How many levels of callers to traverse (max 10) |
| `--limit` | int | 50 | Maximum number of results to print |

Lists the definitions transitively affected by changing `<symbol>` — its callers, their callers,
and so on. Each result reports `depth=N`, its distance from `<symbol>`. Cycles terminate; each
symbol is visited once. No API call.

All three graph commands print `filepath:line symbol`, plus `(reference)` or `(inherit)` when the
relationship is not a plain call. They require an index built with the graph enabled; if it was
not, they say so instead of returning an empty result.

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
