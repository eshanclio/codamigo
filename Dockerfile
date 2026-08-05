# CPU image. Runs the local embedding provider on the pure-Go compute backend, or
# on XLA-CPU if the plugin is installed at runtime.
#
# Model weights are NOT baked in: bge-small-en-v1.5 alone is 133 MB, and which
# model you want is a configuration decision. Mount the cache instead:
#
#   docker build -t codamigo:cpu .
#   docker run --rm -v ~/.codamigo/models:/root/.codamigo/models codamigo:cpu doctor
#
# A builder stage with a C toolchain is mandatory. CGO_ENABLED=0 is not an option
# here: mattn/go-sqlite3 and the tree-sitter grammars in go-code-chunker are both
# cgo, so a static pure-Go build cannot work.

FROM golang:1.26.5-bookworm AS builder

ENV CGO_ENABLED=1

# libsqlite3-dev supplies sqlite3.h, which sqlite-vec's own C sources include.
# The golang image ships gcc but no SQLite headers, so without this the build
# fails with "sqlite-vec.h:7:10: fatal error: sqlite3.h: No such file or
# directory". It is easy to miss on macOS, where the SDK provides the header.
RUN apt-get update \
    && apt-get install -y --no-install-recommends libsqlite3-dev \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src

# Copy the module files first so dependency download is cached independently of
# source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# sqlite_fts5 enables FTS5, which the BM25 half of hybrid search depends on.
RUN go build -tags sqlite_fts5 -o /out/codamigo ./cmd/codamigo/

FROM debian:bookworm-slim

# ca-certificates is needed by `codamigo download-model`; libgomp1 is the OpenMP
# runtime the XLA CPU plugin links against if one is mounted in.
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates libgomp1 \
    && rm -rf /var/lib/apt/lists/*

# Fail fast to the pure-Go backend rather than reaching for GitHub mid-run if no
# PJRT plugin is present. Installing one is an explicit `download-model --xla`.
ENV GOMLX_NO_AUTO_INSTALL=1

COPY --from=builder /out/codamigo /usr/local/bin/codamigo

# The project being indexed is mounted here.
WORKDIR /work

ENTRYPOINT ["codamigo"]
CMD ["doctor"]
