package agenteval_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ChristopherDavenport/agenteval"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentturn"
)

// advantage scores each member by its contains score less the group's
// mean, as an RL producer computes advantages, and counts its calls.
func advantage(calls *atomic.Int32) agenteval.GroupJudge {
	return agenteval.GroupJudgeFunc{JudgeName: "advantage", Fn: func(_ context.Context, group []agenteval.Member, _ agenteval.Task) ([]agenteval.Score, error) {
		calls.Add(1)
		values := make([]float64, len(group))
		mean := 0.0
		for i, m := range group {
			for _, s := range m.Scores {
				if s.Judge == "contains" {
					values[i] = s.Value
				}
			}
			mean += values[i] / float64(len(group))
		}
		out := make([]agenteval.Score, len(group))
		for i := range group {
			if len(group[i].Trajectory.Path) == 0 {
				return nil, errors.New("a member has no trajectory")
			}
			out[i] = agenteval.Score{Value: values[i] - mean, Pass: true}
		}
		return out, nil
	}}
}

// sessionRecord reads the task record and the outcomes of a result's
// session.
func sessionRecord(t *testing.T, store agentsession.Store, res agenteval.Result) (agenteval.TaskRecord, map[string]agenteval.OutcomeDetails) {
	t.Helper()
	s, err := store.Open(context.Background(), res.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	var rec agenteval.TaskRecord
	outcomes := map[string]agenteval.OutcomeDetails{}
	for _, e := range s.Entries() {
		switch v := e.(type) {
		case *agentsession.CustomEntry:
			if v.NS == agenteval.TaskNS {
				if err := json.Unmarshal(v.Data, &rec); err != nil {
					t.Fatal(err)
				}
			}
		case *agentsession.OutcomeEntry:
			score, details, ok := agenteval.ReadOutcome(v)
			if !ok || v.Target != res.Target {
				t.Errorf("outcome %+v", v)
			}
			outcomes[score.Judge] = details
		}
	}
	return rec, outcomes
}

// Issue 40: a runner that samples runs each task N times, in N
// sessions, and scores each group once all of it is judged.
func TestRunnerSamplesAndJudgesGroups(t *testing.T) {
	for _, parallel := range []int{1, 4} {
		store := newStableStore()
		var calls atomic.Int32
		r := basicRunner(store)
		r.Samples, r.Parallel = 3, parallel
		r.GroupJudges = []agenteval.GroupJudge{advantage(&calls)}
		suite := basicSuite(t)
		report, err := r.Run(context.Background(), suite)
		if err != nil {
			t.Fatal(err)
		}
		if len(report.Results) != 3*len(suite.Tasks) || int(calls.Load()) != len(suite.Tasks) {
			t.Fatalf("parallel %d: %d results and %d group calls for %d tasks", parallel, len(report.Results), calls.Load(), len(suite.Tasks))
		}
		sessions := map[string]bool{}
		for i, res := range report.Results {
			task, sample := suite.Tasks[i/3], i%3+1
			if res.Err != nil {
				t.Errorf("%s#%d: %v", res.Task.ID, res.Sample, res.Err)
			}
			if res.Task.ID != task.ID || res.Sample != sample || res.Task.Meta[agenteval.SampleMeta] != string(rune('0'+sample)) {
				t.Errorf("result %d is %s sample %d, meta %v; want %s sample %d", i, res.Task.ID, res.Sample, res.Task.Meta, task.ID, sample)
			}
			sessions[res.SessionID] = true
			if len(res.Scores) != len(r.Judges)+1 || res.Scores[len(res.Scores)-1].Judge != "advantage" {
				t.Errorf("%s#%d: scores %+v", res.Task.ID, res.Sample, res.Scores)
			}
			rec, outcomes := sessionRecord(t, store, res)
			if rec.Sample != sample || rec.Samples != 3 || len(rec.GroupJudges) != 1 || rec.GroupJudges[0] != "advantage" {
				t.Errorf("%s#%d: task record %+v", res.Task.ID, res.Sample, rec)
			}
			if d, ok := outcomes["advantage"]; !ok || d.Sample != sample || d.Task != task.ID {
				t.Errorf("%s#%d: outcomes %+v", res.Task.ID, res.Sample, outcomes)
			}
		}
		if len(sessions) != len(report.Results) {
			t.Errorf("%d sessions for %d results", len(sessions), len(report.Results))
		}
		if sum := report.ByJudge["advantage"]; sum.Count != len(report.Results) || sum.Mean > 1e-9 || sum.Mean < -1e-9 {
			t.Errorf("advantage summary %+v, want every sample counted and a mean of 0", sum)
		}
		// The suite's own tasks are left as they were.
		for _, task := range suite.Tasks {
			if _, ok := task.Meta[agenteval.SampleMeta]; ok {
				t.Errorf("the suite's task %s was given a sample", task.ID)
			}
		}
	}
}

// A runner that does not sample judges each task as a group of one.
func TestRunnerGroupOfOne(t *testing.T) {
	store := newStableStore()
	var calls atomic.Int32
	r := basicRunner(store)
	r.GroupJudges = []agenteval.GroupJudge{advantage(&calls)}
	report, err := r.Run(context.Background(), basicSuite(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, res := range report.Results {
		if res.Sample != 0 || res.Err != nil || res.Scores[len(res.Scores)-1].Judge != "advantage" {
			t.Errorf("%s: sample %d, err %v, scores %+v", res.Task.ID, res.Sample, res.Err, res.Scores)
		}
		if _, ok := res.Task.Meta[agenteval.SampleMeta]; ok {
			t.Errorf("%s: meta %v", res.Task.ID, res.Task.Meta)
		}
	}
	if int(calls.Load()) != len(report.Results) {
		t.Errorf("%d group calls for %d tasks", calls.Load(), len(report.Results))
	}
}

// A group with a sample whose run failed is not group judged, as an RL
// producer drops it, and every other sample says so; JudgeFailedRuns
// judges the failed sample, which completes the group.
func TestRunnerIncompleteGroup(t *testing.T) {
	suite := &agenteval.Suite{Name: "s", Tasks: basicSuite(t).Tasks[:1]}
	for _, judgeFailed := range []bool{false, true} {
		store := newStableStore()
		var calls atomic.Int32
		r := basicRunner(store)
		r.Samples, r.JudgeFailedRuns = 3, judgeFailed
		r.GroupJudges = []agenteval.GroupJudge{advantage(&calls)}
		r.Config = func(task agenteval.Task) agentturn.Config {
			if task.Meta[agenteval.SampleMeta] == "2" {
				return agentturn.Config{Model: failingModel{}}
			}
			return basicConfig(task)
		}
		report, err := r.Run(context.Background(), suite)
		if err != nil {
			t.Fatal(err)
		}
		if judged := calls.Load() == 1; judged != judgeFailed {
			t.Fatalf("JudgeFailedRuns %v: group judged %v", judgeFailed, judged)
		}
		for _, res := range report.Results {
			_, outcomes := sessionRecord(t, store, res)
			if _, ok := outcomes["advantage"]; ok != judgeFailed {
				t.Errorf("JudgeFailedRuns %v: sample %d holds a group outcome %v", judgeFailed, res.Sample, ok)
			}
			if res.Sample == 2 || judgeFailed {
				continue
			}
			if res.Err == nil || !strings.Contains(res.Err.Error(), "group not judged: sample 2 was not judged") {
				t.Errorf("sample %d: err %v, want it to name the unjudged sample", res.Sample, res.Err)
			}
		}
	}
}

// A group judge that fails, or returns a score count other than the
// group's, writes nothing and names itself on every sample.
func TestRunnerGroupJudgeErrors(t *testing.T) {
	suite := &agenteval.Suite{Name: "s", Tasks: basicSuite(t).Tasks[:1]}
	tests := []struct {
		name string
		fn   func(context.Context, []agenteval.Member, agenteval.Task) ([]agenteval.Score, error)
		want string
	}{
		{"an error", func(context.Context, []agenteval.Member, agenteval.Task) ([]agenteval.Score, error) {
			return nil, errors.New("boom")
		}, "group judge broken: boom"},
		{"too few scores", func(context.Context, []agenteval.Member, agenteval.Task) ([]agenteval.Score, error) {
			return []agenteval.Score{{Value: 1}}, nil
		}, "group judge broken: 1 scores for 2 samples"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newStableStore()
			r := basicRunner(store)
			r.Samples = 2
			r.GroupJudges = []agenteval.GroupJudge{agenteval.GroupJudgeFunc{JudgeName: "broken", Fn: tt.fn}}
			report, err := r.Run(context.Background(), suite)
			if err != nil {
				t.Fatal(err)
			}
			for _, res := range report.Results {
				if res.Err == nil || !strings.Contains(res.Err.Error(), tt.want) || len(res.Scores) != len(r.Judges) {
					t.Errorf("sample %d: err %v, %d scores", res.Sample, res.Err, len(res.Scores))
				}
				if _, outcomes := sessionRecord(t, store, res); len(outcomes) != len(r.Judges) {
					t.Errorf("sample %d: outcomes %v", res.Sample, outcomes)
				}
			}
		})
	}
}

// A suite whose task sets the sample key itself is refused rather than
// overwritten.
func TestRunnerRefusesASampleKey(t *testing.T) {
	r := basicRunner(newStableStore())
	r.Samples = 2
	suite := &agenteval.Suite{Name: "s", Tasks: []agenteval.Task{{ID: "t", Instruction: "hi", Meta: map[string]string{agenteval.SampleMeta: "x"}}}}
	if _, err := r.Run(context.Background(), suite); err == nil || !strings.Contains(err.Error(), `Meta["sample"]`) {
		t.Errorf("err = %v", err)
	}
}

// A comparison of two sampling runners pairs results by task and
// sample.
func TestCompareSamples(t *testing.T) {
	a, b := basicRunner(newStableStore()), basicRunner(newStableStore())
	a.Samples, b.Samples = 2, 2
	suite := basicSuite(t)
	c, err := agenteval.Compare(context.Background(), suite, a, b)
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Pairs) != 2*len(suite.Tasks) {
		t.Fatalf("%d pairs for %d tasks of 2 samples", len(c.Pairs), len(suite.Tasks))
	}
	for _, p := range c.Pairs {
		if p.Sample == 0 || p.A.Sample != p.Sample || p.B.Sample != p.Sample || p.A.Task.ID != p.Task || p.B.Task.ID != p.Task {
			t.Errorf("pair %s#%d pairs %s#%d with %s#%d", p.Task, p.Sample, p.A.Task.ID, p.A.Sample, p.B.Task.ID, p.B.Sample)
		}
	}
}
