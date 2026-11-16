BIN     = harness-wiki
VERSION = v0.2.0
LDFLAGS = -s -w -X github.com/gurcuff91/harness-wiki/internal/version.Version=$(VERSION)

.PHONY: build install test vet clean release release-push

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
	@test -z "$$(gofmt -l .)" || (echo "gofmt needs to run on:"; gofmt -l .; exit 1)
	@go vet ./...

# Cut a release: tag HEAD as $(VERSION). Guards, in order — each aborts before anything is tagged:
#   1. the working tree must be clean (no uncommitted changes)
#   2. VERSION must not already exist as a tag (never overwrite a release)
#   3. CHANGELOG.md must have an entry for this version
# The annotated tag's message is this version's CHANGELOG section. Push it with `make release-push`.
release: vet test
	@echo "Preparing release $(VERSION)..."
	@if [ -n "$$(git status --porcelain)" ]; then \
		echo "❌ Working tree is not clean — commit or stash first."; \
		exit 1; \
	fi
	@if git rev-parse -q --verify "refs/tags/$(VERSION)" >/dev/null; then \
		echo "❌ Tag $(VERSION) already exists — bump VERSION in the Makefile first."; \
		exit 1; \
	fi
	@if ! grep -q "\[$(VERSION:v%=%)\]" CHANGELOG.md; then \
		echo "❌ No CHANGELOG.md entry for $(VERSION:v%=%) — document the release first."; \
		exit 1; \
	fi
	@echo "Tagging $(VERSION) at $$(git rev-parse --short HEAD)..."
	@awk '/^## \[$(VERSION:v%=%)\]/{f=1;print;next} /^## \[/{f=0} f' CHANGELOG.md \
		| git tag -a "$(VERSION)" -F -
	@echo "✅ Tagged $(VERSION). Push it with: make release-push"

# Push the release tag to origin (a separate step so the tag can be reviewed first). The tag fires
# .github/workflows/release.yml, which builds the binaries and publishes the GitHub Release.
release-push:
	@if ! git rev-parse -q --verify "refs/tags/$(VERSION)" >/dev/null; then \
		echo "❌ Tag $(VERSION) doesn't exist yet — run 'make release' first."; \
		exit 1; \
	fi
	@echo "Pushing $(VERSION) to origin..."
	git push origin "$(VERSION)"
	@echo "✅ Released $(VERSION)"

clean:
	@rm -f ./$(BIN)
	@rm -rf dist/
