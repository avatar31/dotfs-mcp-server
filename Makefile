BINARY   := dotfs-mcp-server
BIN_DIR  := bin
PKG      := ./cmd

# cgo is mandatory: the C engine links the Tree-sitter grammar.
export CGO_ENABLED := 1

.PHONY: all build test race vet fmt tidy clean run

all: fmt vet test build

build_binary:
	@mkdir -p $(BIN_DIR)
	go build -trimpath -ldflags "-s -w" -o $(BIN_DIR)/$(BINARY) $(PKG)

test:
	go test ./...

race:
	go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

tidy:
	go mod tidy

install: build_binary
	cp $(BIN_DIR)/$(BINARY) /usr/local/bin/$(BINARY)

run: install
	$(BINARY)

clean:
	rm -rf $(BIN_DIR) agent_knowledge
