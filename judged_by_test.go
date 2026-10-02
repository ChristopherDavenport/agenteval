package agenteval_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ChristopherDavenport/agenteval"
	"github.com/ChristopherDavenport/agenteval/judge"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/export"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses/echo"
)

// judgedByLinks returns the judged_by links on the path to a result's
// target, through the reader agentsession gives for them.
func judgedByLinks(t *testing.T, store agentsession.Store, res agenteval.Result) []*agentsession.LinkEntry {
	t.Helper()
	s, err := store.Open(context.Background(), res.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	links, err := s.Judges(s.Leaf())
	if err != nil {
		t.Fatal(err)
	}
	return links
}

// RFC 0001 0.11 gives a judged session a judged_by link to the session
// of its judge, naming the entry the outcome names. A rubric judge runs
// in a session of its own, which a link says is a judge's and not a
// subagent's or a fork's; a deterministic judge has no session and
// writes no link.
func TestRunnerLinksTheJudgeSessions(t *testing.T) {
	ctx := context.Background()
	store := newStableStore()
	verdict, err := json.Marshal(judge.Verdict{Score: 0.5, Pass: true, Reason: "ok"})
	if err != nil {
		t.Fatal(err)
	}
	rubric := judge.Rubric(agentturn.Config{Model: &echo.Adapter{}, ModelName: "echo/judge"}, "Score it.",
		judge.WithStore(store), judge.WithName("quality"),
		judge.WithRender(func(export.Trajectory) (string, error) { return string(verdict), nil }))
	r := basicRunner(store)
	r.Judges = []agenteval.Judge{judge.Contains("contains"), rubric}
	suite := basicSuite(t)
	report, err := r.Run(ctx, suite)
	if err != nil {
		t.Fatal(err)
	}
	for _, res := range report.Results {
		if res.Err != nil {
			t.Fatalf("%s: %v", res.Task.ID, res.Err)
		}
		var judgeSession string
		for _, sc := range res.Scores {
			if sc.Judge == "quality" {
				judgeSession = sc.Session
			} else if sc.Session != "" {
				t.Errorf("%s: judge %s has session %q", res.Task.ID, sc.Judge, sc.Session)
			}
		}
		if judgeSession == "" {
			t.Fatalf("%s: the rubric judge named no session", res.Task.ID)
		}
		links := judgedByLinks(t, store, res)
		if len(links) != 1 || links[0].Rel != agentsession.RelJudgedBy || links[0].Session != judgeSession || links[0].Target != res.Target {
			t.Errorf("%s: judged_by links %+v; want one to %s targeting %s", res.Task.ID, links, judgeSession, res.Target)
		}
		// The link is for the entry the outcome names, and the judge's
		// header names the judged session as its parent.
		j, err := store.Open(ctx, judgeSession)
		if err != nil {
			t.Fatal(err)
		}
		if p := j.Header().ParentSession; p != res.SessionID {
			t.Errorf("%s: the judge's parent is %s, want %s", res.Task.ID, p, res.SessionID)
		}
		s, err := store.Open(ctx, res.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range s.Entries() {
			if o, ok := e.(*agentsession.OutcomeEntry); ok && o.Target != res.Target {
				t.Errorf("%s: outcome targets %s, want %s", res.Task.ID, o.Target, res.Target)
			}
		}
	}
}

// A group judge that ran in a session of its own is linked from each
// member it scored, as a judge of the run is.
func TestRunnerLinksAGroupJudgeSession(t *testing.T) {
	store := newStableStore()
	r := basicRunner(store)
	r.Samples = 2
	r.GroupJudges = []agenteval.GroupJudge{agenteval.GroupJudgeFunc{JudgeName: "rank", Fn: func(_ context.Context, group []agenteval.Member, _ agenteval.Task) ([]agenteval.Score, error) {
		out := make([]agenteval.Score, len(group))
		for i := range out {
			out[i] = agenteval.Score{Value: float64(i), Pass: true, Session: "rank-session"}
		}
		return out, nil
	}}}
	suite := basicSuite(t)
	report, err := r.Run(context.Background(), suite)
	if err != nil {
		t.Fatal(err)
	}
	for _, res := range report.Results {
		if res.Err != nil {
			t.Fatalf("%s#%d: %v", res.Task.ID, res.Sample, res.Err)
		}
		links := judgedByLinks(t, store, res)
		if len(links) != 1 || links[0].Session != "rank-session" || links[0].Target != res.Target {
			t.Errorf("%s#%d: judged_by links %+v", res.Task.ID, res.Sample, links)
		}
	}
}
