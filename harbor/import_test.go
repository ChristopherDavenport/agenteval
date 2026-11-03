package harbor_test

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/ChristopherDavenport/agenteval"
	"github.com/ChristopherDavenport/agenteval/harbor"
	"github.com/ChristopherDavenport/agentsession"
)

// trialSession records a run in store as an agent records one under a
// trial's agent directory, ending for reason, and a subagent's session
// beside it, and returns the run's session and its last entry.
func trialSession(t *testing.T, store agentsession.Store, task, reason string) (string, string) {
	t.Helper()
	ctx := context.Background()
	s, err := store.Create(ctx, agentsession.Header{})
	if err != nil {
		t.Fatal(err)
	}
	child, err := store.Create(ctx, agentsession.Header{ParentSession: s.ID()})
	if err != nil {
		t.Fatal(err)
	}
	var last string
	for _, e := range []agentsession.Entry{
		agentsession.NewRunStart("run_"+task, agentsession.SourceInput, ""),
		agentsession.NewRunEnd("run_"+task, reason, "", nil),
	} {
		if last, err = store.Append(ctx, s.ID(), e); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Append(ctx, child.ID(), agentsession.NewRunStart("run_c", agentsession.SourceInput, "")); err != nil {
		t.Fatal(err)
	}
	return s.ID(), last
}

// trialStore is a store holding one trial's sessions: the run's and
// those it names as their parent.
type trialStore struct {
	*agentsession.MemoryStore
	session string
}

func (s trialStore) List(ctx context.Context, f agentsession.ListFilter) iter.Seq2[agentsession.Summary, error] {
	return func(yield func(agentsession.Summary, error) bool) {
		for sum, err := range s.MemoryStore.List(ctx, f) {
			if err == nil && sum.Header.ID != s.session && sum.Header.ParentSession != s.session {
				continue
			}
			if !yield(sum, err) {
				return
			}
		}
	}
}

// entries counts the entries every session of store holds.
func entries(t *testing.T, store *agentsession.MemoryStore) int {
	t.Helper()
	n := 0
	for sum, err := range store.List(context.Background(), agentsession.ListFilter{}) {
		if err != nil {
			t.Fatal(err)
		}
		s, err := store.Read(context.Background(), sum.Header.ID)
		if err != nil {
			t.Fatal(err)
		}
		n += len(s.Entries())
	}
	return n
}

// outcomes returns the outcome entries a session holds.
func outcomes(t *testing.T, store agentsession.Store, id string) []*agentsession.OutcomeEntry {
	t.Helper()
	s, err := store.(agentsession.Reader).Read(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	var out []*agentsession.OutcomeEntry
	for _, e := range s.Entries() {
		if o, ok := e.(*agentsession.OutcomeEntry); ok {
			out = append(out, o)
		}
	}
	return out
}

// TestImportJob is #66: a Harbor job's rewards are written back onto
// each trial's agent session as outcome entries, a verifier's scores
// (RFC 0001, outcome, kind test) labelled harbor/<reward key> and
// targeting the run's last entry, and the job is read as a Report. An
// errored trial's 0, which Harbor counts, is recorded with the
// exception and left unjudged in the report. A second import writes
// nothing.
func TestImportJob(t *testing.T) {
	ctx := context.Background()
	store := agentsession.NewMemoryStore()
	type trial struct{ session, target string }
	trials := map[string]trial{}
	for name, task := range map[string]string{"hello__a1b2c3d": "hello", "multi__e4f5g6h": "multi", "broken__i7j8k9l": "broken"} {
		reason := "done"
		if task == "broken" {
			reason = "aborted"
		}
		id, target := trialSession(t, store, task, reason)
		trials[name] = trial{id, target}
	}
	// Each trial's store holds its own session and its subagent's; the
	// stores here are one, filtered to the trial's sessions.
	open := func(name string) (agentsession.Store, error) {
		return trialStore{MemoryStore: store, session: trials[name].session}, nil
	}
	report, err := harbor.ImportJob(ctx, os.DirFS("testdata/job"), open)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Results) != 3 {
		t.Fatalf("results = %+v", report.Results)
	}
	byTask := map[string]agenteval.Result{}
	for _, r := range report.Results {
		byTask[r.Task.ID] = r
	}
	hello, multi, broken := byTask["agenteval/hello"], byTask["agenteval/multi"], byTask["agenteval/broken"]
	if hello.SessionID != trials["hello__a1b2c3d"].session || hello.Target != trials["hello__a1b2c3d"].target {
		t.Errorf("hello = %s@%s, want the trial's run", hello.SessionID, hello.Target)
	}
	if len(hello.Scores) != 1 || hello.Scores[0].Judge != "harbor/reward" || hello.Scores[0].Value != 1 || !hello.Scores[0].Pass {
		t.Errorf("hello scores = %+v", hello.Scores)
	}
	if len(multi.Scores) != 2 || multi.Scores[0].Judge != "harbor/reward" || multi.Scores[1].Judge != "harbor/style" || multi.Scores[1].Value != 0.5 || multi.Scores[1].Pass {
		t.Errorf("multi scores = %+v", multi.Scores)
	}
	if len(broken.Scores) != 0 || broken.Err == nil || !strings.Contains(broken.Err.Error(), "AgentTimeoutError") {
		t.Errorf("broken = scores %+v err %v, want unjudged with the exception", broken.Scores, broken.Err)
	}
	if sum := report.ByJudge["harbor/reward"]; sum.Count != 2 || sum.Unjudged != 1 || sum.Mean != 1 {
		t.Errorf("harbor/reward = %+v", sum)
	}
	// The outcomes on the sessions.
	for name, tr := range trials {
		outs := outcomes(t, store, tr.session)
		if len(outs) == 0 {
			t.Errorf("%s: no outcome", name)
		}
		for _, o := range outs {
			if o.Kind != agentsession.OutcomeTest || o.Target != tr.target || !strings.HasPrefix(o.Label, "harbor/") {
				t.Errorf("%s: outcome %+v", name, o)
			}
		}
	}
	errored := outcomes(t, store, trials["broken__i7j8k9l"].session)
	// The 0 is Harbor's count, not a verdict: it carries no pass, which
	// RFC 0001 reads as the judge's (review of #66).
	if len(errored) != 1 || errored[0].Score == nil || *errored[0].Score != 0 || errored[0].Pass != nil || !strings.Contains(string(errored[0].Details), "AgentTimeoutError") {
		t.Errorf("errored trial's outcome = %+v", errored)
	}
	var d agenteval.OutcomeDetails
	if len(errored) == 1 {
		if err := json.Unmarshal(errored[0].Details, &d); err != nil || d.Error == "" || d.Task != "agenteval/broken" {
			t.Errorf("errored trial's details = %s", errored[0].Details)
		}
	}
	// Read back from the store, the report compares as it was imported.
	again, err := agenteval.ResultAt(ctx, store, broken.SessionID, broken.Target)
	if err != nil || len(again.Scores) != 0 || again.Err == nil {
		t.Errorf("the errored trial read back = %+v, %v", again, err)
	}
	// Idempotent.
	before := entries(t, store)
	if _, err := harbor.ImportJob(ctx, os.DirFS("testdata/job"), open); err != nil {
		t.Fatal(err)
	}
	if after := entries(t, store); after != before {
		t.Errorf("a second import appended %d entries", after-before)
	}
}

// A job that ran a task more than once, Harbor's n_attempts, holds one
// trial per attempt: each is a sample of the task, numbered from 1 in
// trial order, so two imported jobs pair attempt with attempt rather
// than every attempt with the first (review of #66).
func TestImportJobNumbersAttempts(t *testing.T) {
	ctx := context.Background()
	store := agentsession.NewMemoryStore()
	trial := func(task string) []byte {
		return []byte(`{"task_name":"` + task + `","verifier_result":{"rewards":{"reward":1}}}`)
	}
	job := fstest.MapFS{
		"hello__aaa/result.json": {Data: trial("agenteval/hello")},
		"hello__bbb/result.json": {Data: trial("agenteval/hello")},
		"other__ccc/result.json": {Data: trial("agenteval/other")},
	}
	sessions := map[string]string{}
	for _, name := range []string{"hello__aaa", "hello__bbb", "other__ccc"} {
		sessions[name], _ = trialSession(t, store, name, "done")
	}
	open := func(name string) (agentsession.Store, error) {
		return trialStore{MemoryStore: store, session: sessions[name]}, nil
	}
	report, err := harbor.ImportJob(ctx, job, open)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range report.Results {
		if r.Err != nil {
			t.Errorf("%s: %v", r.Task.Meta["harbor.trial"], r.Err)
		}
		got = append(got, fmt.Sprintf("%s#%d(%s)", r.Task.ID, r.Sample, r.Task.Meta[agenteval.SampleMeta]))
	}
	if want := "agenteval/hello#1(1) agenteval/hello#2(2) agenteval/other#0()"; strings.Join(got, " ") != want {
		t.Errorf("results = %s, want %s", strings.Join(got, " "), want)
	}
}

// Two imported jobs compare though each trial's session is in a store
// of its own: the resolver reuses the open the import was given, keyed
// by the trial (review of #65).
func TestImportedJobsCompare(t *testing.T) {
	ctx := context.Background()
	stores := map[string]*agentsession.MemoryStore{}
	trial := func(reward string) []byte {
		return []byte(`{"task_name":"agenteval/hello","verifier_result":{"rewards":{"reward":` + reward + `}}}`)
	}
	jobs := map[string]fstest.MapFS{
		"a": {"hello__aaa/result.json": {Data: trial("1")}},
		"b": {"hello__bbb/result.json": {Data: trial("0")}},
	}
	for _, name := range []string{"hello__aaa", "hello__bbb"} {
		stores[name] = agentsession.NewMemoryStore()
		trialSession(t, stores[name], name, "done")
	}
	open := func(name string) (agentsession.Store, error) { return stores[name], nil }
	a, err := harbor.ImportJob(ctx, jobs["a"], open)
	if err != nil {
		t.Fatal(err)
	}
	b, err := harbor.ImportJob(ctx, jobs["b"], open)
	if err != nil {
		t.Fatal(err)
	}
	c, err := agenteval.CompareReports(ctx, a, b, harbor.Resolver(open), harbor.Resolver(open))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Pairs) != 1 || c.Pairs[0].Delta["harbor/reward"] != -1 {
		t.Errorf("pairs = %+v", c.Pairs)
	}
}
