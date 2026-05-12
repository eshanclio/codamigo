.PHONY: build test fmt vet lint fuzz tidy clean \
        test-chunker test-langs test-config test-embedder \
        test-store test-walker test-indexer test-query test-mcp

# CGO_ENABLED=1 is required: langs/markdown.go compiles C sources via cgo.
export CGO_ENABLED := 1

# Build tags: sqlite_fts5 enables FTS5 full-text search in go-sqlite3.
TAGS := -tags sqlite_fts5

build:
	go build $(TAGS) -o codamigo ./cmd/codamigo/

test:
	go test $(TAGS) ./...

test-chunker:
	go test $(TAGS) ./chunker/...

test-langs:
	go test $(TAGS) ./langs/...

test-config:
	go test $(TAGS) ./config/...

test-embedder:
	go test $(TAGS) ./embedder/...

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

# Run a single test by name: make run-test TEST=TestChunkFile_Metadata PKG=./chunker/...
run-test:
	go test $(TAGS) $(PKG) -run $(TEST) -v

fuzz:
	go test $(TAGS) ./chunker/... -fuzz=FuzzChunkFile -fuzztime=30s

fmt:
	go fmt -w .

vet:
	go vet $(TAGS) ./...

lint:
	golangci-lint run ./...

tidy:
	go mod tidy

clean:
	go clean ./...
