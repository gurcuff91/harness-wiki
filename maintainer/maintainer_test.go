package maintainer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gurcuff91/harness/agent"
)

// fakeModel is an OpenAI-compatible endpoint that answers every request with a fixed reply and
// records what it was sent: the system prompt, the tool names, the model id and the user messages.
type fakeModel struct {
	*httptest.Server
	mu      sync.Mutex
	systems []string
	tools   [][]string
	models  []string
	users   []string
	gate    chan struct{} // when non-nil, every request waits for it
	reply   string
}

func newFakeModel(t *testing.T) *fakeModel {
	t.Helper()
	m := &fakeModel{reply: "the maintainer's report"}
	m.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Write([]byte(`{"data":[{"id":"fake-1"},{"id":"fake-2"}]}`))
			return
		}
		var body struct {
			Model    string `json:"model"`
			Messages []struct {
				Role    string `json:"role"`
				Content any    `json:"content"`
			} `json:"messages"`
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		m.mu.Lock()
		m.models = append(m.models, body.Model)
		var names []string
		for _, tl := range body.Tools {
			names = append(names, tl.Function.Name)
		}
		m.tools = append(m.tools, names)
		for _, msg := range body.Messages {
			s, _ := msg.Content.(string)
			switch msg.Role {
			case "system":
				m.systems = append(m.systems, s)
			case "user":
				m.users = append(m.users, s)
			}
		}
		gate, reply := m.gate, m.reply
		m.mu.Unlock()
		if gate != nil {
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		b, _ := json.Marshal(reply)
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%s}}]}\n\n", b)
		fmt.Fprint(w, `data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":4}}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(m.Close)
	return m
}

// withProvider registers the fake as the ONLY provider and gives the test its own empty settings,
// so nothing of the developer's ~/.harness (or a connected real provider) can leak in.
func withProvider(t *testing.T, m *fakeModel) (name string) {
	t.Helper()
	isolate(t)
	name = "fake-" + strings.ToLower(strings.NewReplacer("/", "-", " ", "-").Replace(t.Name()))
	if err := agent.NewOpenAIProvider(name, m.URL); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { agent.UnregisterCustomProvider(name) })
	return name
}

func wikiDir(t *testing.T, agentsMD string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "w")
	os.MkdirAll(filepath.Join(dir, "raw"), 0o755)
	os.MkdirAll(filepath.Join(dir, "wiki"), 0o755)
	os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte(agentsMD), 0o644)
	return dir
}

func TestExecuteRunsTheMaintainerInTheWikiFolderWithItsOwnSchema(t *testing.T) {
	m := newFakeModel(t)
	name := withProvider(t, m)
	dir := wikiDir(t, "# THE-WIKIS-OWN-SCHEMA-MARKER")
	// the server's cwd has an AGENTS.md of its own: it must NOT be what the maintainer reads
	cwd := t.TempDir()
	os.WriteFile(filepath.Join(cwd, "AGENTS.md"), []byte("# THE-SERVERS-CWD-MARKER"), 0o644)
	t.Chdir(cwd)

	r := New(name + "/fake-1")
	defer r.Close()
	out, err := r.Execute(context.Background(), Run{Slug: "w", Dir: dir, Op: OpQuery, Prompt: "what is the SGW?"})
	if err != nil || out != "the maintainer's report" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	sys := strings.Join(m.systems, "\n")
	for _, want := range []string{"THE-WIKIS-OWN-SCHEMA-MARKER", "maintainer of the wiki", "raw/ is IMMUTABLE", dir} {
		if !strings.Contains(sys, want) {
			t.Errorf("the system prompt lacks %q:\n%s", want, sys)
		}
	}
	if strings.Contains(sys, "THE-SERVERS-CWD-MARKER") {
		t.Fatal("the maintainer read the AGENTS.md of the directory the SERVER started in, not the wiki's")
	}
	if len(m.users) == 0 || !strings.Contains(m.users[len(m.users)-1], "what is the SGW?") {
		t.Errorf("the model never received the prompt: %v", m.users)
	}
	if got := m.models[len(m.models)-1]; got != "fake-1" {
		t.Errorf("the model id sent = %q, want fake-1", got)
	}
}

func TestTheMaintainerHasTheFileToolsAndNothingThatDelegates(t *testing.T) {
	m := newFakeModel(t)
	name := withProvider(t, m)
	r := New(name + "/fake-1")
	defer r.Close()
	if _, err := r.Execute(context.Background(), Run{Dir: wikiDir(t, "# s"), Op: OpLint, Prompt: "lint"}); err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, n := range m.tools[len(m.tools)-1] {
		got[n] = true
	}
	for _, want := range []string{"Bash", "Read", "Write", "Edit", "Fetch"} {
		if !got[want] {
			t.Errorf("the maintainer lacks %s: %v", want, m.tools[len(m.tools)-1])
		}
	}
	for _, denied := range []string{"Subagent", "MemoWrite", "MemoSearch", "MemoDelete", "Schedule", "ScheduleList", "ScheduleDelete", "ColleagueList", "ColleagueAsk"} {
		if got[denied] {
			t.Errorf("the maintainer has %s, which it must not", denied)
		}
	}
}

func TestEachOperationGetsItsOwnBudgetAndTheBudgetsGrowWithTheWork(t *testing.T) {
	want := map[Op]int{OpIngest: 100, OpLint: 100, OpQuery: 40, OpInit: 15}
	for op, n := range want {
		if budget(op) != n {
			t.Errorf("%s budget = %d, want %d", op, budget(op), n)
		}
	}
	if budget("unknown") != Iterations[OpIngest] {
		t.Error("an unknown operation must get the largest budget, not zero")
	}
	if !(Iterations[OpInit] < Iterations[OpQuery] && Iterations[OpQuery] < Iterations[OpIngest]) {
		t.Error("budgets must grow with the work: init < query < ingest")
	}
}

func TestModelResolution(t *testing.T) {
	m := newFakeModel(t)
	name := withProvider(t, m)
	defer func() {}()

	// an explicit model on the run wins over the runner's, which wins over "first available"
	r := New(name + "/fake-2")
	defer r.Close()
	if got, err := r.ResolveModel("other/x"); err != nil || got != "other/x" {
		t.Errorf("the run's own model: %q %v", got, err)
	}
	if got, err := r.ResolveModel("  "); err != nil || got != name+"/fake-2" {
		t.Errorf("the runner's model: %q %v", got, err)
	}
	// no model given anywhere: the first model of the first active provider
	r2 := New("")
	defer r2.Close()
	got, err := r2.ResolveModel("")
	if err != nil || got != name+"/fake-1" {
		t.Errorf("first available: %q %v, want %s/fake-1", got, err, name)
	}
}

func TestNoProviderIsAClearError(t *testing.T) {
	isolate(t)
	r := New("")
	defer r.Close()
	ran := false
	_, err := r.Execute(context.Background(), Run{Dir: wikiDir(t, "# s"), Op: OpQuery, Prompt: "q"})
	_ = ran
	if err == nil || !strings.Contains(err.Error(), "no model is available") || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Fatalf("err = %v, want ErrNoModel naming the env vars that activate a provider", err)
	}
}

// A model that does not exist is the SDK's own error, naming the model — not a silent fallback.
func TestAnUnknownModelFailsClearly(t *testing.T) {
	m := newFakeModel(t)
	name := withProvider(t, m)
	r := New(name + "/not-a-model")
	defer r.Close()
	_, err := r.Execute(context.Background(), Run{Dir: wikiDir(t, "# s"), Op: OpQuery, Prompt: "q"})
	if err == nil || !strings.Contains(err.Error(), "not-a-model") {
		t.Fatalf("err = %v, want an error naming the unknown model", err)
	}
	if len(m.users) != 0 {
		t.Fatal("the model was called although the requested one does not exist")
	}
}

// Many maintainers at once: none waits for another.
func TestExecuteRunsConcurrently(t *testing.T) {
	m := newFakeModel(t)
	name := withProvider(t, m)
	m.gate = make(chan struct{})
	r := New(name + "/fake-1")
	defer r.Close()
	const n = 6
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := r.Execute(context.Background(), Run{Dir: wikiDir(t, "# s"), Op: OpQuery, Prompt: "q"})
			errs <- err
		}()
	}
	deadline := time.Now().Add(8 * time.Second)
	for {
		m.mu.Lock()
		got := len(m.models)
		m.mu.Unlock()
		if got >= n {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d maintainers reached the model at once: they are being serialised", got, n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	close(m.gate)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("a maintainer failed: %v", err)
		}
	}
}

// Cancelling the caller's context stops the maintainer; Shutdown cancels the ones still running.
func TestCancellationAndShutdownStopRunningMaintainers(t *testing.T) {
	m := newFakeModel(t)
	name := withProvider(t, m)
	m.gate = make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-m.gate:
		default:
			close(m.gate)
		}
	})
	r := New(name + "/fake-1")

	run := func(ctx context.Context) <-chan error {
		ch := make(chan error, 1)
		go func() {
			_, err := r.Execute(ctx, Run{Dir: wikiDir(t, "# s"), Op: OpIngest, Prompt: "go"})
			ch <- err
		}()
		return ch
	}
	waitArrived := func(n int) {
		deadline := time.Now().Add(5 * time.Second)
		for {
			m.mu.Lock()
			got := len(m.models)
			m.mu.Unlock()
			if got >= n {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("the maintainer never reached the model (%d/%d)", got, n)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	first := run(ctx)
	waitArrived(1)
	cancel()
	select {
	case err := <-first:
		if err == nil {
			t.Error("a cancelled maintainer returned success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling the caller did not stop the maintainer")
	}

	second := run(context.Background())
	waitArrived(2)
	r.Shutdown(5 * time.Second)
	select {
	case err := <-second:
		if err == nil {
			t.Error("a maintainer stopped by shutdown returned success")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Shutdown did not stop the running maintainer")
	}
	// nothing new starts once shut down, and the model is not called
	before := len(m.models)
	if _, err := r.Execute(context.Background(), Run{Dir: wikiDir(t, "# s"), Op: OpQuery, Prompt: "q"}); err == nil || !strings.Contains(err.Error(), "shutting down") {
		t.Errorf("after shutdown: %v, want a shutting-down refusal", err)
	}
	if len(m.models) != before {
		t.Error("the model was called during shutdown")
	}
	r.Close()
}

func TestShutdownGivesUpAfterTheGracePeriod(t *testing.T) {
	r := New("x/y")
	_, end, ok := r.begin(context.Background())
	if !ok {
		t.Fatal("begin refused")
	}
	defer end()
	t0 := time.Now()
	r.Shutdown(200 * time.Millisecond) // a run that never stops
	if el := time.Since(t0); el > 2*time.Second {
		t.Errorf("Shutdown took %v: it must give up after its grace period", el)
	}
}

// A provider that is not connected is never chosen as "the first available model", whatever it
// happens to list: only an ACTIVE provider can run a maintainer.
func TestAnInactiveProviderIsNeverChosenAsTheDefault(t *testing.T) {
	// no providers are active here (empty settings, no API keys): nothing is available, even
	// though the registry lists every built-in provider
	isolate(t)
	r := New("")
	defer r.Close()
	if got, err := r.ResolveModel(""); err == nil {
		t.Fatalf("ResolveModel chose %q although no provider is active, want ErrNoModel", got)
	} else if !errors.Is(err, ErrNoModel) {
		t.Fatalf("err = %v, want ErrNoModel", err)
	}
}
