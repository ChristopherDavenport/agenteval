package agenteval_test

import (
	"context"
	"testing"

	"github.com/ChristopherDavenport/agenteval"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/export"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/session"
	"github.com/ChristopherDavenport/agentturn/tools/agent"
	"github.com/ChristopherDavenport/openresponses"
	"github.com/ChristopherDavenport/openresponses/echo"
)

// miniRate prices the echo model as echoRate does and the child's
// echo-mini at a tenth of it, so a call priced under the wrong model
// shows in the total.
func miniRate(model string, u openresponses.Usage) (float64, bool) {
	if model == "echo/echo-mini" {
		usd, _ := flatRate(model, u)
		return usd / 10, true
	}
	return echoRate(model, u)
}

// delegating is the echo configuration whose one tool is a subagent on
// echo-mini, recorded in a session of its own through the run's
// recorder: the echo adapter calls it with the prompt, and the child
// answers.
func delegating(rec *session.Recorder) agentturn.Config {
	cfg := echoConfig()
	child := agentturn.Config{Name: "specialist", Description: "answers", Model: &echo.Adapter{}, ModelName: "echo/echo-mini"}
	cfg.Tools = []agenttool.Tool{agent.New(child, agent.WithObserver(rec.Observe), agent.WithRunContext(rec.ChildContext))}
	return cfg
}

// unreadable is a store that reads no session without a hold: a child
// read so is one the reader cannot read.
type unreadable struct{ *stableStore }

func (unreadable) Read(_ context.Context, id string) (*agentsession.Session, error) {
	return nil, agentsession.ErrNoSession
}

// TestRunnerCostCountsDelegatedWork is #56: a run's cost includes the
// work its path delegated to subsessions, as RFC 0001 (link) counts it
// and the exported document's total_cost_usd does, each child's calls
// at the model that served them in the child, while its usage stays
// the path's own, as the document's token totals do. A child that
// cannot be read or priced is named and left out.
func TestRunnerCostCountsDelegatedWork(t *testing.T) {
	suite := &agenteval.Suite{Name: "s", Tasks: []agenteval.Task{{ID: "one", Instruction: "hello"}}}
	run := func(t *testing.T, store agentsession.Store, cost func(string, openresponses.Usage) (float64, bool)) (agenteval.Result, *agentsession.Session) {
		t.Helper()
		r := &agenteval.Runner{
			Store:      store,
			ConfigWith: func(_ agenteval.Task, rec *session.Recorder) agentturn.Config { return delegating(rec) },
			Cost:       cost,
		}
		report, err := r.Run(context.Background(), suite)
		if err != nil {
			t.Fatal(err)
		}
		res := report.Results[0]
		if res.Err != nil {
			t.Fatal(res.Err)
		}
		s, err := store.Open(context.Background(), res.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		return res, s
	}
	// pathCost is the parent's path alone, priced.
	pathCost := func(t *testing.T, s *agentsession.Session, leaf string) (float64, openresponses.Usage) {
		t.Helper()
		var usd float64
		var u openresponses.Usage
		for _, c := range agentsession.ModelCalls(s.Path(leaf)) {
			if c.Usage == nil {
				continue
			}
			p, ok := miniRate(c.Model, *c.Usage)
			if !ok {
				t.Fatalf("the parent's call on %s is not priced", c.Model)
			}
			usd += p
			u.InputTokens += c.Usage.InputTokens
			u.OutputTokens += c.Usage.OutputTokens
		}
		return usd, u
	}

	t.Run("priced", func(t *testing.T) {
		store := newStableStore()
		res, s := run(t, store, miniRate)
		own, u := pathCost(t, s, res.Target)
		if res.Usage.InputTokens != u.InputTokens || res.Usage.OutputTokens != u.OutputTokens {
			t.Errorf("usage = %+v, want the path's own %+v", res.Usage, u)
		}
		sub := res.Subagents
		if sub == nil || sub.Sessions != 1 || sub.Usage.InputTokens == 0 || sub.ModelCalls == 0 || sub.CostUSD == nil || *sub.CostUSD <= 0 {
			t.Fatalf("subagents = %+v", sub)
		}
		if len(sub.Unread) != 0 || len(sub.Unpriced) != 0 {
			t.Errorf("subagents = %+v, want the child read and priced", sub)
		}
		if res.CostUSD == nil || *res.CostUSD != own+*sub.CostUSD {
			t.Errorf("cost = %v, want the path's %v and the child's %v", res.CostUSD, own, *sub.CostUSD)
		}
		tr, err := export.At(s, res.Target)
		if err != nil {
			t.Fatal(err)
		}
		doc, err := export.ToATIF(tr, export.Options{Cost: miniRate, Subsessions: func(id string) (*agentsession.Session, error) {
			return store.Read(context.Background(), id)
		}})
		if err != nil {
			t.Fatal(err)
		}
		m := doc.FinalMetrics
		if m == nil || m.TotalCostUSD == nil || m.TotalPromptTokens == nil {
			t.Fatalf("the document has no totals: %+v", m)
		}
		if *res.CostUSD != *m.TotalCostUSD || res.Usage.InputTokens != *m.TotalPromptTokens {
			t.Errorf("cost %v, %d in; the document %v, %d in", *res.CostUSD, res.Usage.InputTokens, *m.TotalCostUSD, *m.TotalPromptTokens)
		}
	})

	t.Run("unpriced", func(t *testing.T) {
		res, s := run(t, newStableStore(), echoRate)
		own, _ := pathCost(t, s, res.Target)
		sub := res.Subagents
		if sub == nil || len(sub.Unpriced) != 1 || sub.Sessions != 1 {
			t.Fatalf("subagents = %+v, want the child named unpriced", sub)
		}
		if res.CostUSD == nil || *res.CostUSD != own {
			t.Errorf("cost = %v, want the path's own %v, the child left out", res.CostUSD, own)
		}
	})

	t.Run("unread", func(t *testing.T) {
		res, s := run(t, unreadable{newStableStore()}, miniRate)
		own, _ := pathCost(t, s, res.Target)
		sub := res.Subagents
		if sub == nil || len(sub.Unread) != 1 || sub.Unread[0].Session == "" || sub.Unread[0].Error == "" || sub.Sessions != 0 {
			t.Fatalf("subagents = %+v, want the child named unread", sub)
		}
		if res.CostUSD == nil || *res.CostUSD != own {
			t.Errorf("cost = %v, want the path's own %v, the child left out", res.CostUSD, own)
		}
	})

	t.Run("none", func(t *testing.T) {
		r := basicRunner(newStableStore())
		report, err := r.Run(context.Background(), basicSuite(t))
		if err != nil {
			t.Fatal(err)
		}
		for _, res := range report.Results {
			if res.Subagents != nil {
				t.Errorf("%s: subagents = %+v for a run that delegated nothing", res.Task.ID, res.Subagents)
			}
		}
	})
}
