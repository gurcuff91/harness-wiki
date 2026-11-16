package api

import (
	_ "embed"
	"net/http"
	"strings"

	"github.com/gurcuff91/harness-wiki/internal/version"
)

// docsHTML is the Scalar API reference UI — a single self-contained HTML page that loads the
// OpenAPI document from /api/docs/openapi.json (same origin, so no CORS) and renders it with the
// Scalar CDN bundle. No build step, no extra assets. It is the same page harness serves.
const docsHTML = `<!doctype html>
<html lang="en">
  <head>
    <title>harness-wiki API Reference</title>
    <meta charset="utf-8" />
    <meta name="viewport" content="width=device-width, initial-scale=1" />
  </head>
  <body>
    <script id="api-reference" data-url="/api/docs/openapi.json"></script>
    <script src="https://cdn.jsdelivr.net/npm/@scalar/api-reference"></script>
  </body>
</html>`

//go:embed openapi.json
var openapiTemplate string

// OpenAPI returns the OpenAPI 3.0 document with the binary's version and the address the server is
// reachable at filled in. (The address is the one the request arrived on, so it is right behind
// --host/--port and --host 0.0.0.0 alike — a fixed value would not be.)
func OpenAPI(version, addr string) string {
	s := strings.ReplaceAll(openapiTemplate, "__VERSION__", version)
	return strings.ReplaceAll(s, "__ADDR__", addr)
}

// docs serves the Scalar reference page.
func (h *handler) docs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(docsHTML))
}

// openapi serves the OpenAPI document.
func (h *handler) openapi(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(OpenAPI(version.Version, r.Host)))
}
