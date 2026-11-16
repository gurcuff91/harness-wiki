// Package version holds the build version, injected with -ldflags from the Makefile.
package version

// Version is "dev" for a plain `go build` and the release tag for a `make build`.
var Version = "dev"
