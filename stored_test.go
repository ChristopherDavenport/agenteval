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

// storedRun records a run with no task record, as a session another
// harness wrote, scored value by judge, and returns it as a report's
// result names it.
func storedRun(t *testing.T, store agentsession.Store, task string, value float64) agenteval.Result {
	t.Helper()
	ctx := context.Background()
	s, err := store.Create(ctx, agentsession.Header{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(ctx, s.ID(), agentsession.NewRunStart("run_"+task, agentsession.SourceInput, "")); err != nil {
		t.Fatal(err)
	}
	target, err := store.Append(ctx, s.ID(), agentsession.NewRunEnd("run_"+task, "done", "", nil))
	if err != nil {
		t.Fatal(err)
	}
	out, err := agenteval.NewOutcome(agenteval.Score{Judge: "judge", Value: value, Pass: value >= 1}, target, task)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(ctx, s.ID(), out); err != nil {
		t.Fatal(err)
	}
	return agenteval.Result{Task: agenteval.Task{ID: task}, SessionID: s.ID(), Target: target}
}

// A session with no task record, every one Harbor's import reads, is
// paired by the task its report names (review of #59).
func TestCompareReportsWithoutTaskRecords(t *testing.T) {
	store := newStableStore()
	a := &agenteval.Report{Suite: "s", Results: []agenteval.Result{storedRun(t, store, "one", 1), storedRun(t, store, "two", 0)}}
	b := &agenteval.Report{Suite: "s", Results: []agenteval.Result{storedRun(t, store, "one", 0), storedRun(t, store, "two", 1)}}
	c, err := agenteval.CompareReports(context.Background(), a, b, store, store)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Pairs) != 2 || c.Pairs[0].Task != "one" || c.Pairs[1].Task != "two" {
		t.Fatalf("pairs = %+v", c.Pairs)
	}
	if c.Pairs[0].Delta["judge"] != -1 || c.Pairs[1].Delta["judge"] != 1 || c.Pairs[1].B.SessionID != b.Results[1].SessionID {
		t.Errorf("deltas %v %v, b of two %s", c.Pairs[0].Delta, c.Pairs[1].Delta, c.Pairs[1].B.SessionID)
	}
}
