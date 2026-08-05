.PHONY: build test fmt vet lint tidy clean \
        test-config test-localembed test-arch \
        test-store test-walker test-indexer test-query test-mcp \
        test-race bench-localembed docker docker-cuda

# CGO_ENABLED=1 is required: go-code-chunker dependency compiles C sources via cgo.
export CGO_ENABLED := 1

# Build tags: sqlite_fts5 enables FTS5 full-text search in go-sqlite3.
TAGS := -tags sqlite_fts5

build:
	go build $(TAGS) -o codamigo ./cmd/codamigo/

test:
	go test $(TAGS) ./...

test-config:
	go test $(TAGS) ./config/...

test-store:
	go test $(TAGS) ./store/...

test-walker:
	go test $(TAGS) ./walker/...

test-indexer:
	go test $(TAGS) ./indexer/...

test-query:
	go test $(TAGS) ./query/...

test-mcp:
	go test $(TAGS) ./mcp/...

test-localembed:
	go test $(TAGS) ./localembed/...

# Asserts the one-way internal dependency order documented in AGENTS.md.
test-arch:
	go test $(TAGS) ./internal/arch/...

# Inference on the pure-Go compute backend is skipped under -race: it trips
# checkptr inside GoMLX's own unsafe matmul kernel, which aborts the whole test
# binary. The XLA backend still covers the concurrency paths.
test-race:
	go test $(TAGS) -race ./...

bench-localembed:
	go test $(TAGS) -bench . -benchmem ./localembed/...

# Run a single test by name: make run-test TEST=TestSearch_Basic PKG=./query/...
run-test:
	go test $(TAGS) $(PKG) -run $(TEST) -v

fmt:
	go fmt ./...

vet:
	go vet $(TAGS) ./...

lint:
	golangci-lint run ./...

tidy:
	go mod tidy

# Model weights are not baked in; mount ~/.codamigo/models at run time.
docker:
	docker build -t codamigo:cpu .

# UNVERIFIED: needs a linux/amd64 host with an NVIDIA GPU. See Dockerfile.cuda.
docker-cuda:
	docker build -f Dockerfile.cuda -t codamigo:cuda .

clean:
	go clean ./...
