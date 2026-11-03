package agenteval_test

import (
	"bytes"
	"context"
	"sync/atomic"
	"testing"

	"github.com/ChristopherDavenport/agenteval"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
	"github.com/ChristopherDavenport/openresponses/echo"
)

// counting is the echo adapter counting the calls it answers.
type counting struct{ calls *atomic.Int32 }

func (c counting) CreateStream(ctx context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	c.calls.Add(1)
	return (&echo.Adapter{}).CreateStream(ctx, req, sink)
}

// TestCompareAgainstAPinnedBaseline is #59: a candidate is compared
// against stored results, here a baseline each task's ref pins (RFC
// 0002, Refs), without running the baseline again. Each side is read
// back from its store, so a report read from JSON compares as the one
// Run returned, and the baseline's side is the pinned session itself.
func TestCompareAgainstAPinnedBaseline(t *testing.T) {
	ctx := context.Background()
	suite := basicSuite(t)
	store := newStableStore()
	var calls atomic.Int32
	a := basicRunner(store)
	a.Config = func(task agenteval.Task) agentturn.Config {
		cfg := basicConfig(task)
		cfg.Model = counting{&calls}
		return cfg
	}
	base, err := a.Run(ctx, suite)
	if err != nil {
		t.Fatal(err)
	}
	for _, res := range base.Results {
		if err := store.UpdateRef(ctx, "eval/baseline/"+res.Task.ID, agentsession.RefTarget{}, agentsession.RefTarget{Session: res.SessionID, Entry: res.Target}, "baseline"); err != nil {
			t.Fatal(err)
		}
	}
	ran := calls.Load()

	pinned := &agenteval.Report{Suite: suite.Name, Manifest: suite.Manifest}
	for _, task := range suite.Tasks {
		ref, err := store.ResolveRef(ctx, "eval/baseline/"+task.ID)
		if err != nil {
			t.Fatal(err)
		}
		res, err := agenteval.ResultAt(ctx, store, ref.Session, ref.Entry, agenteval.WithCost(flatRate))
		if err != nil {
			t.Fatal(err)
		}
		pinned.Results = append(pinned.Results, *res)
	}
	for i, res := range pinned.Results {
		was := base.Results[i]
		if res.Task.ID != was.Task.ID || res.Target != was.Target || res.Reason != was.Reason || res.Runs != was.Runs || len(res.Scores) != len(was.Scores) {
			t.Errorf("stored %s = %+v, ran %+v", was.Task.ID, res, was)
		}
		if res.Usage.InputTokens != was.Usage.InputTokens || res.CostUSD == nil || *res.CostUSD != *was.CostUSD || res.ModelCalls != was.ModelCalls {
			t.Errorf("stored %s usage %+v cost %v, ran %+v %v", was.Task.ID, res.Usage, res.CostUSD, was.Usage, was.CostUSD)
		}
	}

	b := basicRunner(store)
	b.Config = func(task agenteval.Task) agentturn.Config {
		cfg := echoConfig()
		cfg.Instructions = "Be thorough."
		cfg.Tools = nil
		return cfg
	}
	cand, err := b.Run(ctx, suite)
	if err != nil {
		t.Fatal(err)
	}
	// The candidate's report as a file holds what the sessions hold and
	// no more: compared from JSON it compares the same.
	var buf bytes.Buffer
	if err := cand.WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}
	read, err := agenteval.ReadReport(&buf)
	if err != nil {
		t.Fatal(err)
	}
	c, err := agenteval.CompareReports(ctx, pinned, read, store, store)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != ran {
		t.Errorf("the baseline's model was called %d more times", calls.Load()-ran)
	}
	if len(c.Pairs) != 2 || c.Unjudged != 0 {
		t.Fatalf("comparison = %+v", c)
	}
	for i, p := range c.Pairs {
		if p.A.SessionID != base.Results[i].SessionID || p.A.Target != base.Results[i].Target {
			t.Errorf("%s: A is %s@%s, want the pinned %s@%s", p.Task, p.A.SessionID, p.A.Target, base.Results[i].SessionID, base.Results[i].Target)
		}
	}
	if c.ByJudge["contains"] != -0.5 || c.ByJudge["tool_called:upper"] != -0.5 || c.ByJudge["exact"] != 0 {
		t.Errorf("by judge = %v", c.ByJudge)
	}
	if gd := c.Pairs[0].Config; gd.Instructions == nil || gd.Instructions.A != "Be brief." || gd.Instructions.B != "Be thorough." {
		t.Errorf("greet config = %+v", gd)
	}
}
