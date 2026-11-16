BIN     = harness-wiki
VERSION = v0.1.0
LDFLAGS = -s -w -X github.com/gurcuff91/harness-wiki/internal/version.Version=$(VERSION)

.PHONY: build install test vet clean

build:
	@echo "Building $(BIN) $(VERSION)..."
	@go build -ldflags "$(LDFLAGS)" -o ./$(BIN) ./cmd/harness-wiki
	@echo "✅ Built: ./$(BIN)"

install: build
	@mkdir -p ~/go/bin
	@cp ./$(BIN) ~/go/bin/$(BIN)
	@echo "✅ Installed to ~/go/bin/$(BIN)"

test:
	@go test -race -count=1 ./...

vet:
	@gofmt -l . && go vet ./...

clean:
	@rm -f ./$(BIN)
