package api

import (
	_ "embed"
	"net/http"
	"strings"

	"github.com/gurcuff91/harness-wiki/internal/version"
)

//go:embed openapi.json
var openapiTemplate string

// openapi serves the OpenAPI document. The version is the binary's own, so the document never
// disagrees with the server that serves it.
func (h *handler) openapi(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(strings.Replace(openapiTemplate, "__VERSION__", version.Version, 1)))
}

// OpenAPI returns the document with the given version filled in (for tests and tooling).
func OpenAPI(version string) string {
	return strings.Replace(openapiTemplate, "__VERSION__", version, 1)
}
