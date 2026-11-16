// Package maintainer runs the AI side of a wiki: a sub-agent, built on the harness SDK, whose
// working directory is the wiki's own folder — so it reads THAT wiki's AGENTS.md as project
// context and maintains the wiki by its rules. The wiki package (the store) never calls a model;
// everything that does lives here, one layer above it.
package maintainer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/gurcuff91/harness/agent"
	"github.com/gurcuff91/harness/agent/resources"
	"github.com/gurcuff91/harness/agent/store"
	"github.com/gurcuff91/harness/agent/tools"
)

// Op names an operation, for the iteration budget and the prompt.
type Op string

const (
	OpInit   Op = "init"
	OpIngest Op = "ingest"
	OpQuery  Op = "query"
	OpLint   Op = "lint"
)

// String makes Op print as its name.
func (o Op) String() string { return string(o) }

// Run is one request for a maintainer.
type Run struct {
	Slug   string
	Dir    string // the wiki's folder: the maintainer's working directory
	Op     Op
	Prompt string
	// Model overrides the runner's model for this one run ("provider/model"); empty = the
	// runner's own (see Runner.Model).
	Model string
}

// ErrNoModel is returned when no model can be resolved: no provider is active. The harness SDK
// activates a provider from its usual environment variables (ANTHROPIC_API_KEY, OPENAI_API_KEY,
// MINIMAX_API_KEY, OLLAMA_CLOUD_API_KEY, OPENCODE_GO_API_KEY, OLLAMA_URL) or from
// ~/.harness/credentials.json when that exists.
var ErrNoModel = errors.New("no model is available: connect a provider (for example set ANTHROPIC_API_KEY or OPENAI_API_KEY)")

// systemPrompt is the maintainer's system prompt. The wiki's own AGENTS.md reaches it as project
// context (its working directory IS the wiki folder) and says how THIS wiki is organised; this
// prompt only fixes what is true of every wiki.
const systemPrompt = `You are the maintainer of the wiki in your working directory — a persistent, interlinked collection of markdown pages that you build and keep current from the sources the user gives you. Knowledge is compiled into the wiki once and kept up to date; it is not re-derived from raw documents on every question.

Rules that hold for every wiki:
1. Read AGENTS.md first (it is in your project context): it is this wiki's schema, and its conventions win over your own habits.
2. raw/ is IMMUTABLE. Read the sources there; never modify, rename or delete them.
3. wiki/ is yours. You write and maintain every page in it, and you keep wiki/index.md and wiki/log.md up to date on every operation. Log entries start with "## [YYYY-MM-DD] op | title".
4. Work autonomously and completely; never ask questions. Make reasonable assumptions and say what you assumed.
5. Finish with a short report of what you did or found. That report is all the caller sees.`

// Iterations is the ReAct budget per operation. An ingest or a lint can touch dozens of pages; a
// query reads a few and answers; adapting a schema edits one file.
var Iterations = map[Op]int{
	OpIngest: 100,
	OpLint:   100,
	OpQuery:  40,
	OpInit:   15,
}

// ShutdownGrace is how long Close waits for running maintainers to stop after cancelling them.
const ShutdownGrace = 5 * time.Second

// disallowed is what a maintainer may NOT use: it cannot launch other sub-agents, manage
// schedules, write the user's memory, or call colleagues. It keeps Bash, Read, Write, Edit and
// Fetch (and WebSearch where the SDK provides it): it maintains files and may fill a gap in the
// data. There are no Wiki* tools to deny — this process is an HTTP API, not an agent with tools.
func disallowed() []string {
	return []string{
		tools.ToolSubagent,
		tools.ToolMemoWrite, tools.ToolMemoSearch, tools.ToolMemoDelete,
		tools.ToolSchedule, tools.ToolScheduleList, tools.ToolScheduleDelete,
		tools.ToolColleagueList, tools.ToolColleagueAsk,
	}
}

// Runner runs maintainers and tracks them, so Close can cancel the ones still running.
type Runner struct {
	// Model is the default "provider/model". Empty = the first model any active provider offers.
	Model string
	// ThinkingLevel is passed to the agent ("" = the SDK's own default).
	ThinkingLevel string

	mu      sync.Mutex
	closed  bool
	cancels map[int]context.CancelFunc
	next    int
	wg      sync.WaitGroup

	// lister is a long-lived SDK agent used only to look at the model registry. It has no
	// prompt and no tools of its own; each maintainer gets a fresh agent (see Execute) because
	// the SDK fixes the system prompt and the iteration budget per AGENT, not per session.
	listerOnce sync.Once
	lister     *agent.Agent
}

// New returns a Runner using model as its default ("" = first available).
func New(model string) *Runner {
	return &Runner{Model: model, cancels: map[int]context.CancelFunc{}}
}

func (r *Runner) modelLister() *agent.Agent {
	r.listerOnce.Do(func() {
		r.lister = agent.New(agent.AgentOptions{Store: store.NewInMemoryStore(), ResourceLoader: resources.NilLoader{}})
	})
	return r.lister
}

// begin registers a running operation; the returned func ends it. ok is false once the runner is
// closed — nothing new starts during shutdown.
func (r *Runner) begin(parent context.Context) (ctx context.Context, end func(), ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, nil, false
	}
	ctx, cancel := context.WithCancel(parent)
	id := r.next
	r.next++
	r.cancels[id] = cancel
	r.wg.Add(1)
	return ctx, func() {
		cancel()
		r.mu.Lock()
		delete(r.cancels, id)
		r.mu.Unlock()
		r.wg.Done()
	}, true
}

// Close cancels every running maintainer and waits up to ShutdownGrace for them to finish, then
// releases the model-listing agent. It is safe to call more than once.
func (r *Runner) Close() error {
	r.Shutdown(ShutdownGrace)
	r.mu.Lock()
	ag := r.lister
	r.mu.Unlock()
	if ag != nil {
		return ag.Close()
	}
	return nil
}

// Shutdown is Close's cancel-and-wait half, with a configurable grace period.
func (r *Runner) Shutdown(grace time.Duration) {
	r.mu.Lock()
	r.closed = true
	cancels := make([]context.CancelFunc, 0, len(r.cancels))
	for _, c := range r.cancels {
		cancels = append(cancels, c)
	}
	r.mu.Unlock()
	for _, c := range cancels {
		c()
	}
	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(grace):
	}
}

// ResolveModel returns the model a run uses: the run's own, else the runner's, else the first
// model any active provider offers. It fails with ErrNoModel when none is usable. The explicit
// choices are returned as given — the SDK validates them when the session is created, with an
// error that names the provider and says why.
func (r *Runner) ResolveModel(explicit string) (string, error) {
	if m := strings.TrimSpace(explicit); m != "" {
		return m, nil
	}
	if m := strings.TrimSpace(r.Model); m != "" {
		return m, nil
	}
	for _, pm := range r.modelLister().ListModels("") {
		// The SDK documents Models as nil for an inactive provider, so Active is redundant today;
		// it is kept so that an SDK that ever lists models for a provider that is not connected
		// still cannot get one chosen as the default.
		if pm.Active && len(pm.Models) > 0 {
			return pm.Models[0].Model, nil
		}
	}
	return "", ErrNoModel
}

// Execute runs one maintainer for one operation and returns its final report. It stops when ctx
// ends or the runner is closed.
func (r *Runner) Execute(ctx context.Context, run Run) (string, error) {
	ctx, end, ok := r.begin(ctx)
	if !ok {
		return "", errors.New("wiki: the server is shutting down")
	}
	defer end()

	model, err := r.ResolveModel(run.Model)
	if err != nil {
		return "", err
	}
	// One SDK agent per run: the SDK fixes the system prompt, the iteration budget and the
	// blocked tools per AGENT. Its resource loader is rooted at the WIKI folder — not at wherever
	// the server was started — so the maintainer reads that wiki's own AGENTS.md. The agent is
	// cheap (in-memory session store, no MCP, no scheduler) and goes away with the run.
	ag := agent.New(agent.AgentOptions{
		SystemPrompt:    systemPrompt,
		MaxIterations:   budget(run.Op),
		ThinkingLevel:   r.ThinkingLevel,
		Store:           store.NewInMemoryStore(),
		ResourceLoader:  resources.NewFileResourceLoader(run.Dir),
		DisallowedTools: disallowed(),
	})
	defer ag.Close()
	sess, err := ag.NewSession(run.Dir, model)
	if err != nil {
		return "", fmt.Errorf("start the maintainer: %w", err)
	}
	defer sess.Close()
	out, err := sess.PromptAndWait(ctx, run.Prompt)
	// The SDK treats a cancelled turn as a user Stop — not an error — and returns ("", nil). For a
	// maintainer that would read as "finished, with an empty report": a half-done ingest reported
	// as a success, its pending marker cleared. A cancelled run must be an error.
	if ctx.Err() != nil {
		return out, fmt.Errorf("the maintainer was stopped before it finished: %w", context.Cause(ctx))
	}
	return out, err
}

func budget(op Op) int {
	if n := Iterations[op]; n > 0 {
		return n
	}
	return Iterations[OpIngest]
}
