// harness-wiki is an HTTP server for LLM-maintained wikis: persistent, interlinked markdown
// knowledge bases that a model builds and keeps current from the sources you ingest (the "LLM
// Wiki" pattern). It is built on the harness SDK, so any provider harness supports works — chosen
// the way harness chooses: from ~/.harness when it exists and, on a machine without it, from
// harness's own environment variables (ANTHROPIC_API_KEY, OPENAI_API_KEY, MINIMAX_API_KEY,
// OLLAMA_CLOUD_API_KEY, OPENCODE_GO_API_KEY, OLLAMA_URL).
//
//	harness-wiki serve [--host 127.0.0.1] [--port 8787] [--model provider/model]
//
// The wikis live in ./harness-wiki/<slug>/, in the directory the server is started in.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/gurcuff91/harness-wiki/api"
	"github.com/gurcuff91/harness-wiki/internal/version"
	"github.com/gurcuff91/harness-wiki/maintainer"
	"github.com/gurcuff91/harness-wiki/service"
	"github.com/gurcuff91/harness-wiki/wiki"
)

const (
	defaultHost = "127.0.0.1"
	defaultPort = 8787
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

const usage = `harness-wiki — LLM-maintained wikis over HTTP

Usage:
  harness-wiki serve [--host HOST] [--port PORT] [--model PROVIDER/MODEL]
  harness-wiki version

serve flags:
  --host    address to listen on (default 127.0.0.1; use 0.0.0.0 to publish)
  --port    port to listen on (default 8787)
  --model   "provider/model" the maintainer runs on (default: the first model any
            active provider offers)

The wikis are stored in ./harness-wiki/ in the directory the server is started in.
`

// run is main without the process exit, so it can be tested.
func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "serve":
		return serve(args[1:], stdout, stderr)
	case "version", "--version", "-v":
		fmt.Fprintln(stdout, "harness-wiki", version.Version)
		return 0
	case "help", "--help", "-h":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "harness-wiki: unknown command %q\n\n%s", args[0], usage)
		return 2
	}
}

// config is the parsed `serve` command line.
type config struct {
	host  string
	port  int
	model string
}

func parseServe(args []string, stderr io.Writer) (config, error) {
	var c config
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&c.host, "host", defaultHost, "address to listen on")
	fs.IntVar(&c.port, "port", defaultPort, "port to listen on")
	fs.StringVar(&c.model, "model", "", "provider/model the maintainer runs on")
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if fs.NArg() > 0 {
		return c, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if c.port < 0 || c.port > 65535 {
		return c, fmt.Errorf("--port %d is not a valid port (0-65535)", c.port)
	}
	return c, nil
}

func serve(args []string, stdout, stderr io.Writer) int {
	c, err := parseServe(args, stderr)
	if err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(stderr, "harness-wiki:", err)
		}
		return 2
	}

	store, err := wiki.OpenFS("")
	if err != nil {
		fmt.Fprintln(stderr, "harness-wiki:", err)
		return 1
	}
	ai := maintainer.New(c.model)
	svc := service.New(store, ai)

	ln, err := net.Listen("tcp", net.JoinHostPort(c.host, strconv.Itoa(c.port)))
	if err != nil {
		fmt.Fprintln(stderr, "harness-wiki: cannot listen:", err)
		return 1
	}
	srv := &http.Server{Handler: api.New(svc), ReadHeaderTimeout: 10 * time.Second}

	fmt.Fprintf(stdout, "harness-wiki %s listening on http://%s — wikis in %s\n", version.Version, ln.Addr(), store.Root())
	if c.model != "" {
		fmt.Fprintf(stdout, "maintainer model: %s\n", c.model)
	}
	if c.host != "127.0.0.1" && c.host != "localhost" && c.host != "::1" {
		fmt.Fprintln(stderr, "harness-wiki: warning: listening beyond localhost and the API has no authentication — "+
			"anyone who can reach this port can read and change the wikis, make the server copy files it can read, and spend the model's tokens")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	select {
	case err := <-errc:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(stderr, "harness-wiki:", err)
			return 1
		}
	case <-ctx.Done():
	}

	// Graceful shutdown: stop taking requests, cancel the maintainers still running so they stop
	// spending tokens, wait for them to release their wiki locks (a normal exit must not leave a
	// wiki busy for the lock's whole heartbeat), then close the store.
	fmt.Fprintln(stdout, "harness-wiki: shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ai.Shutdown(maintainer.ShutdownGrace)
	svc.Close(maintainer.ShutdownGrace)
	_ = srv.Shutdown(shutdownCtx)
	_ = store.Close()
	_ = ai.Close()
	return 0
}
