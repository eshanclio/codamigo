# codamigo Settings

## Config File Locations

| Scope | Path | Purpose |
|-------|------|---------|
| User-level | `~/.codamigo/global_settings.yml` | API keys, default model — shared across projects |
| Project-level | `.codamigo/settings.yml` | Include/exclude patterns, project-specific overrides |

Merge order (later wins): built-in defaults → global file → project file → env vars → CLI flags.

## Key YAML Fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `embedding_model` | string | — | Embedding model identifier |
| `embedding_base_url` | string | — | API base URL |
| `embedding_dimensions` | int | — | Vector dimensions (must match model) |
| `embedding_max_batch_size` | int | 256 | Chunks per API call |
| `embedding_rate_limit` | float | — | Requests/second sustained rate |
| `embedding_rate_burst` | int | — | Max burst above sustained rate |
| `include_patterns` | []string | [] | Glob patterns to include; empty = include all |
| `exclude_patterns` | []string | [] | Additional gitignore-style exclude rules |
| `index_concurrency` | int | 20 | Files indexed in parallel |
| `max_file_size` | int | 1048576 | Skip files larger than N bytes; 0 = no limit |
| `stale_refresh_threshold` | int | 10 | Max changed result files re-indexed in place per search before the rest are flagged stale |
| `enable_graph` | bool | true | Extract the code graph and expose `callers`/`callees`/`impact`. Set `false` to skip the edge tables and hide those tools |

Changing `embedding_model` or `embedding_dimensions` requires `codamigo reset` then `codamigo index`.

Setting `enable_graph: false` after indexing drops each file's edges as that file is re-indexed, so
the graph decays rather than freezing. Re-enable it and re-index to rebuild a complete graph.

## Environment Variables

| Variable | Description |
|----------|-------------|
| `CODAMIGO_API_KEY` | Embedding API key |
| `CODAMIGO_MODEL` | Embedding model name |
| `CODAMIGO_BASE_URL` | Embedding API base URL |
| `CODAMIGO_DIMENSIONS` | Embedding vector dimensions |

## Embedding Provider Examples

**OpenAI:** `text-embedding-3-small` (1536 dims) or `text-embedding-3-large` (3072 dims)
```yaml
embedding_base_url: https://api.openai.com/v1
embedding_model: text-embedding-3-small
embedding_dimensions: 1536
```

**Voyage AI (recommended for code):** `voyage-code-3` (1024 dims)
```yaml
embedding_base_url: https://api.voyageai.com/v1
embedding_model: voyage-code-3
embedding_dimensions: 1024
```

**Ollama (local, no API key needed):**
```yaml
embedding_base_url: http://localhost:11434/v1
embedding_model: nomic-embed-text
embedding_dimensions: 768
```
