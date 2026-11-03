package agenteval_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/ChristopherDavenport/agenteval"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/export"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/session"
	"github.com/ChristopherDavenport/openresponses"
)

// guarded is a model wrapped in a moderation guard that refuses every
// call after the provider billed it, before anything of the answer
// opened: agentturn records the call as an agentturn:model_blocked
// entry carrying the usage.
type guarded struct{}

func (guarded) CreateStream(_ context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	em := openresponses.NewEmitter(sink, openresponses.NewResponse(req))
	em.Response().Usage = &openresponses.Usage{InputTokens: 11, OutputTokens: 3, TotalTokens: 14}
	if err := em.Start(); err != nil {
		return err
	}
	return fmt.Errorf("%w: the provider's filter refused the request", agentturn.ErrGuard)
}

// TestRunnerUsageCountsPaidRecords is #58: a custom entry whose data
// carries a usage records model calls the path paid for, as many as
// its attempts, whatever its namespace (RFC 0001, custom). The result
// counts a guard's refusal and a product's own paid record, as the
// exported document's final metrics do.
func TestRunnerUsageCountsPaidRecords(t *testing.T) {
	const ns = "product:classifier"
	store := newStableStore()
	r := &agenteval.Runner{
		Store: store,
		ConfigWith: func(_ agenteval.Task, rec *session.Recorder) agentturn.Config {
			cfg := echoConfig()
			cfg.Model = guarded{}
			var once sync.Once
			cfg.BeforeModelCall = func(ctx context.Context, _ *openresponses.Request) (err error) {
				once.Do(func() {
					_, err = rec.Annotate(ctx, ns, map[string]any{
						"usage":    openresponses.Usage{InputTokens: 100, OutputTokens: 20, TotalTokens: 120},
						"attempts": 2,
						"model":    "echo/echo-1",
					})
				})
				return err
			}
			return cfg
		},
		Cost: echoRate,
	}
	report, err := r.Run(context.Background(), &agenteval.Suite{Name: "s", Tasks: []agenteval.Task{{ID: "one", Instruction: "hello"}}})
	if err != nil {
		t.Fatal(err)
	}
	res := report.Results[0]
	if res.Reason != agentturn.ReasonStopped {
		t.Fatalf("reason = %s, err = %v", res.Reason, res.Err)
	}
	s, err := store.Open(context.Background(), res.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	blocked := 0
	for _, e := range s.Path(res.Target) {
		if c, ok := e.(*agentsession.CustomEntry); ok && c.NS == session.ModelBlockedNS {
			blocked++
		}
	}
	if blocked != 1 {
		t.Fatalf("%d model_blocked entries on the path: %s", blocked, entryTypes(s))
	}
	if res.Usage.InputTokens != 111 || res.Usage.OutputTokens != 23 || res.Usage.TotalTokens != 134 {
		t.Errorf("usage = %+v, want the paid record's and the refused call's", res.Usage)
	}
	if res.ModelCalls != 3 {
		t.Errorf("model calls = %d, want the record's two attempts and the refused call", res.ModelCalls)
	}
	tr, err := export.At(s, res.Target)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := export.ToATIF(tr, export.Options{Cost: echoRate})
	if err != nil {
		t.Fatal(err)
	}
	m := doc.FinalMetrics
	if m == nil || m.TotalPromptTokens == nil || m.TotalCostUSD == nil || m.TotalSteps == nil {
		t.Fatalf("the document has no totals: %+v", m)
	}
	if res.Usage.InputTokens != *m.TotalPromptTokens || res.Usage.OutputTokens != *m.TotalCompletionTokens || res.ModelCalls != *m.TotalSteps {
		t.Errorf("usage %+v and %d calls, the document %d in, %d out, %d steps", res.Usage, res.ModelCalls, *m.TotalPromptTokens, *m.TotalCompletionTokens, *m.TotalSteps)
	}
	if res.CostUSD == nil || *res.CostUSD != *m.TotalCostUSD {
		t.Errorf("cost %v, the document %v", res.CostUSD, *m.TotalCostUSD)
	}
}

// holds counts the sessions a store holds open: every Open takes a
// hold, and Release lets one go.
type holds struct {
	mu    sync.Mutex
	opens map[string]int
	// ever is every Open, released or not.
	ever int
}

func (h *holds) open(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.opens == nil {
		h.opens = map[string]int{}
	}
	h.opens[id]++
	h.ever++
}

func (h *holds) Release(id string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.opens, id)
	return nil
}

func (h *holds) held() map[string]int {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := map[string]int{}
	for id, n := range h.opens {
		out[id] = n
	}
	return out
}

// openOnly is a store that reads only through Open and holds what it
// opens, as one with no agentsession.Reader does, and lets a session
// go through agentsession.Releaser.
type openOnly struct {
	agentsession.Store
	*holds
}

func (s openOnly) Open(ctx context.Context, id string) (*agentsession.Session, error) {
	s.open(id)
	return s.Store.Open(ctx, id)
}

// reading is a store that reads without a hold through
// agentsession.Reader, and counts what is opened through Open.
type reading struct {
	*stableStore
	*holds
}

func (s reading) Open(ctx context.Context, id string) (*agentsession.Session, error) {
	s.open(id)
	return s.stableStore.Open(ctx, id)
}

// TestCompareHoldsNoSession is #64: a comparison only reads the runs'
// sessions, so it takes no hold on them. A store that reads without
// one is read so; one that can only Open is let go of afterwards.
func TestCompareHoldsNoSession(t *testing.T) {
	for _, tc := range []struct {
		name  string
		store func() (agentsession.Store, *holds)
	}{
		{"reader", func() (agentsession.Store, *holds) {
			h := &holds{}
			return reading{newStableStore(), h}, h
		}},
		{"open only", func() (agentsession.Store, *holds) {
			h := &holds{}
			return openOnly{newStableStore(), h}, h
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, h := tc.store()
			c, err := agenteval.Compare(context.Background(), basicSuite(t), basicRunner(store), basicRunner(store))
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range c.Pairs {
				if p.Config.Empty() != true {
					t.Errorf("%s: config = %+v", p.Task, p.Config)
				}
			}
			if held := h.held(); len(held) != 0 {
				t.Errorf("sessions still held after the comparison: %v", held)
			}
			if _, ok := store.(agentsession.Reader); ok && h.ever != 0 {
				t.Errorf("a store that reads without a hold was opened %d times", h.ever)
			}
		})
	}
}
