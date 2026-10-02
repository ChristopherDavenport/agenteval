package agenteval_test

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
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

// sessionNames lists the store's session names, sorted.
func sessionNames(t *testing.T, store agentsession.Store) []string {
	t.Helper()
	var names []string
	for sum, err := range store.List(context.Background(), agentsession.ListFilter{WithNames: true}) {
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, sum.Name)
	}
	sort.Strings(names)
	return names
}

// TestRunnerResumesAGroup is #51: a batch whose last sample failed is
// resumed by running that sample alone, through Meta["sample"], and
// the group is finished from the store by JudgeGroups. Before it a
// single sample was refused while Samples was set, a rerun without
// Samples was a group of one recorded as sample 0, every sample's
// session was named by the task alone, and nothing group judged
// samples already in the store.
func TestRunnerResumesAGroup(t *testing.T) {
	ctx := context.Background()
	suite := &agenteval.Suite{Name: "s", Tasks: basicSuite(t).Tasks[:1]}
	task := suite.Tasks[0]
	store := newStableStore()
	var calls atomic.Int32
	r := basicRunner(store)
	r.Samples = 3
	r.GroupJudges = []agenteval.GroupJudge{advantage(&calls)}
	r.Config = func(task agenteval.Task) agentturn.Config {
		if task.Meta[agenteval.SampleMeta] == "3" {
			return agentturn.Config{Model: failingModel{}}
		}
		return basicConfig(task)
	}
	// The batch: sample 3's inference fails, so the group is not judged.
	first, err := r.Run(ctx, suite)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Results) != 3 || first.Results[2].Err == nil || calls.Load() != 0 {
		t.Fatalf("the batch: %d results, sample 3 err %v, %d group calls", len(first.Results), first.Results[2].Err, calls.Load())
	}
	// Each sample's session is told apart in a listing.
	if got := sessionNames(t, store); strings.Join(got, " ") != "greet#1 greet#2 greet#3" {
		t.Errorf("names = %v", got)
	}

	// Before the resume the group cannot be finished, and JudgeGroups
	// says so rather than leaving it out: the two judged members carry
	// the missing sample on Err, and nothing is written.
	if partial, err := r.JudgeGroups(ctx, suite); err != nil {
		t.Fatal(err)
	} else if len(partial.Results) != 2 || calls.Load() != 0 {
		t.Errorf("an incomplete group: %d results, %d group calls", len(partial.Results), calls.Load())
	} else {
		for _, res := range partial.Results {
			if res.Err == nil || !strings.Contains(res.Err.Error(), "sample 3 has no judged run in the store") || len(res.Scores) != len(r.Judges) {
				t.Errorf("incomplete group member %d: err %v, %d scores", res.Sample, res.Err, len(res.Scores))
			}
		}
	}

	// The resume: sample 3 alone, as the task's Meta names it, under the
	// same runner. It is numbered as it says and is not a group of one.
	r.Config = basicConfig
	resume := &agenteval.Suite{Name: "s", Tasks: []agenteval.Task{withMeta(task, agenteval.SampleMeta, "3")}}
	second, err := r.Run(ctx, resume)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Results) != 1 {
		t.Fatalf("the resume ran %d results, want sample 3 alone", len(second.Results))
	}
	res := second.Results[0]
	if res.Err != nil || res.Sample != 3 || res.Task.Meta[agenteval.SampleMeta] != "3" || len(res.Scores) != len(r.Judges) {
		t.Errorf("resumed sample: err %v, sample %d, meta %v, %d scores", res.Err, res.Sample, res.Task.Meta, len(res.Scores))
	}
	rec, outcomes := sessionRecord(t, store, res)
	if rec.Sample != 3 || rec.Samples != 3 || len(rec.GroupJudges) != 1 {
		t.Errorf("resumed sample's record %+v", rec)
	}
	if _, ok := outcomes["advantage"]; ok || calls.Load() != 0 {
		t.Errorf("the resumed sample was group judged alone: outcomes %v, %d group calls", outcomes, calls.Load())
	}
	for _, d := range outcomes {
		if d.Sample != 3 {
			t.Errorf("outcome details %+v, want sample 3", d)
		}
	}
	if got := sessionNames(t, store); strings.Join(got, " ") != "greet#1 greet#2 greet#3 greet#3" {
		t.Errorf("names = %v", got)
	}

	// The group step over the store: each sample's latest judged
	// session, the failed one passed over, judged together once.
	report, err := r.JudgeGroups(ctx, suite)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Results) != 3 || calls.Load() != 1 {
		t.Fatalf("JudgeGroups: %d results, %d group calls", len(report.Results), calls.Load())
	}
	want := map[int]string{1: first.Results[0].SessionID, 2: first.Results[1].SessionID, 3: res.SessionID}
	for k, m := range report.Results {
		if m.Err != nil {
			t.Errorf("member %d: %v", k, m.Err)
		}
		if m.Sample != k+1 || m.Task.ID != task.ID || m.SessionID != want[k+1] || m.Target == "" || m.Reason != agentturn.ReasonDone {
			t.Errorf("member %d = %s#%d in %s at %q, reason %s; want sample %d in %s", k, m.Task.ID, m.Sample, m.SessionID, m.Target, m.Reason, k+1, want[k+1])
		}
		// The judges' scores read back, then the group's.
		if len(m.Scores) != len(r.Judges)+1 || m.Scores[0].Judge != "contains" || m.Scores[len(m.Scores)-1].Judge != "advantage" {
			t.Errorf("member %d scores %+v", k, m.Scores)
		}
		_, outcomes := sessionRecord(t, store, m)
		if d, ok := outcomes["advantage"]; !ok || d.Sample != k+1 || d.Task != task.ID {
			t.Errorf("member %d outcomes %+v", k, outcomes)
		}
	}
	if sum := report.ByJudge["advantage"]; sum.Count != 3 || sum.Unjudged != 0 || sum.Mean > 1e-9 || sum.Mean < -1e-9 {
		t.Errorf("advantage summary %+v", sum)
	}
	// The failed session holds no group outcome, and a second pass
	// finds every group judged and does nothing.
	if _, outcomes := sessionRecord(t, store, first.Results[2]); len(outcomes) != 0 {
		t.Errorf("the failed sample's session holds outcomes %v", outcomes)
	}
	again, err := r.JudgeGroups(ctx, suite)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Results) != 0 || calls.Load() != 1 {
		t.Errorf("a second JudgeGroups: %d results, %d group calls", len(again.Results), calls.Load())
	}
	// A task with no run in the store is one result carrying the
	// error; the finished group is left out.
	missing := &agenteval.Suite{Name: "s", Tasks: basicSuite(t).Tasks}
	report, err = r.JudgeGroups(ctx, missing)
	if err != nil || len(report.Results) != 1 || calls.Load() != 1 {
		t.Fatalf("a suite with an unrun task: %d results, %d group calls, %v", len(report.Results), calls.Load(), err)
	}
	if res := report.Results[0]; res.Task.ID != missing.Tasks[1].ID || res.Err == nil || strings.Count(res.Err.Error(), "has no judged run in the store") != 1 || !strings.Contains(res.Err.Error(), "sample 1, 2, 3 has no judged run in the store") {
		t.Errorf("unrun task: %s, err %v", res.Task.ID, res.Err)
	}

	// A sample run again after the group was judged lacks the group's
	// outcome; the group is judged again as a whole, through the resume
	// suite's own task, and the other members get a second outcome.
	again2, err := r.Run(ctx, &agenteval.Suite{Name: "s", Tasks: []agenteval.Task{withMeta(task, agenteval.SampleMeta, "2")}})
	if err != nil || len(again2.Results) != 1 || again2.Results[0].Err != nil {
		t.Fatalf("rerun of sample 2: %v, %+v", err, again2.Results)
	}
	rejudged, err := r.JudgeGroups(ctx, &agenteval.Suite{Name: "s", Tasks: []agenteval.Task{withMeta(task, agenteval.SampleMeta, "2")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rejudged.Results) != 3 || calls.Load() != 2 {
		t.Fatalf("after rerunning sample 2: %d results, %d group calls", len(rejudged.Results), calls.Load())
	}
	for k, res := range rejudged.Results {
		if res.Err != nil || res.Sample != k+1 || res.Task.Meta[agenteval.SampleMeta] != strconv.Itoa(k+1) {
			t.Errorf("rejudged member %d: err %v, sample %d, meta %v", k, res.Err, res.Sample, res.Task.Meta)
		}
	}
	if _, outcomes := sessionRecord(t, store, rejudged.Results[1]); outcomes["advantage"].Sample != 2 {
		t.Errorf("rerun sample 2's group outcome %+v", outcomes["advantage"])
	}
	if sum := rejudged.ByJudge["advantage"]; sum.Count != 3 || sum.Unjudged != 0 {
		t.Errorf("the group judge's summary %+v", sum)
	}

	// A resume suite that names the task once per sample it ran again
	// judges the group once.
	twice := &agenteval.Suite{Name: "s", Tasks: []agenteval.Task{withMeta(task, agenteval.SampleMeta, "1"), withMeta(task, agenteval.SampleMeta, "3")}}
	if _, err := r.Run(ctx, twice); err != nil {
		t.Fatal(err)
	}
	once, err := r.JudgeGroups(ctx, twice)
	if err != nil {
		t.Fatal(err)
	}
	if len(once.Results) != 3 || calls.Load() != 3 {
		t.Errorf("a resume suite naming the task twice: %d results, %d group calls", len(once.Results), calls.Load())
	}
}

// withMeta returns task with one Meta key set, its Meta copied.
func withMeta(task agenteval.Task, key, value string) agenteval.Task {
	meta := make(map[string]string, len(task.Meta)+1)
	for k, v := range task.Meta {
		meta[k] = v
	}
	meta[key] = value
	task.Meta = meta
	return task
}

// A sample that is not one of 1..Samples is refused with a clear error,
// and JudgeGroups needs group judges and a store.
func TestRunnerRefusesABadSample(t *testing.T) {
	task := basicSuite(t).Tasks[0]
	tests := []struct {
		value string
		want  string
	}{
		{"x", `Meta["sample"] = "x"`},
		{"0", `Meta["sample"] = "0"`},
		{"4", `Meta["sample"] = "4"`},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			r := basicRunner(newStableStore())
			r.Samples = 3
			suite := &agenteval.Suite{Name: "s", Tasks: []agenteval.Task{withMeta(task, agenteval.SampleMeta, tt.value)}}
			_, err := r.Run(context.Background(), suite)
			if err == nil || !strings.Contains(err.Error(), tt.want) || !strings.Contains(err.Error(), "1 to 3") {
				t.Errorf("err = %v", err)
			}
		})
	}
	r := basicRunner(newStableStore())
	if _, err := r.JudgeGroups(context.Background(), &agenteval.Suite{Name: "s", Tasks: []agenteval.Task{task}}); err == nil || !strings.Contains(err.Error(), "no group judges") {
		t.Errorf("no group judges: %v", err)
	}
}
