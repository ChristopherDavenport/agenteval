package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agenteval"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/jsonl"
)

// record writes a scored run into the jsonl store at dir, as a runner
// or an agent under Harbor leaves one, and returns its session and
// target. A negative value writes no score.
func record(t *testing.T, dir, task string, value float64) (string, string) {
	t.Helper()
	ctx := context.Background()
	store, err := jsonl.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	// Run identity in a harness setting's form, as dax's
	// dax.workspace, which differs from store to store.
	s, err := store.Create(ctx, agentsession.Header{Extra: map[string]json.RawMessage{"dax.workspace": json.RawMessage(strconv.Quote(dir))}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(ctx, s.ID(), agentsession.NewRunStart("run_1", agentsession.SourceInput, "")); err != nil {
		t.Fatal(err)
	}
	target, err := store.Append(ctx, s.ID(), agentsession.NewRunEnd("run_1", "done", "", nil))
	if err != nil {
		t.Fatal(err)
	}
	if value >= 0 {
		out, err := agenteval.NewOutcome(agenteval.Score{Judge: "judge", Value: value, Pass: value >= 1}, target, task)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Append(ctx, s.ID(), out); err != nil {
			t.Fatal(err)
		}
	}
	return s.ID(), target
}

// cli runs a command line and returns its standard output, failing the
// test on a non-zero status.
func cli(t *testing.T, args ...string) []byte {
	t.Helper()
	var out, errb bytes.Buffer
	if code := run(context.Background(), args, &out, &errb); code != 0 {
		t.Fatalf("agenteval %s: exit %d: %s", strings.Join(args, " "), code, errb.String())
	}
	return out.Bytes()
}

// TestCommands is #65: a report over stored runs, named by ref or by
// session, a comparison of two reports read again from their stores,
// a run exported as ATIF, and a Harbor job imported onto its trials'
// sessions and compared with another.
func TestCommands(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	storeA, storeB := filepath.Join(tmp, "a"), filepath.Join(tmp, "b")
	idA, targetA := record(t, storeA, "one", 1)
	idB, _ := record(t, storeB, "one", 0)
	{
		store, err := jsonl.Open(storeA)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.UpdateRef(ctx, "eval/baseline/one", agentsession.RefTarget{}, agentsession.RefTarget{Session: idA, Entry: targetA}, "baseline"); err != nil {
			t.Fatal(err)
		}
		store.Close()
	}

	var a, b agenteval.Report
	if err := json.Unmarshal(cli(t, "report", "-store", storeA, "-suite", "s", "eval/baseline/one"), &a); err != nil {
		t.Fatal(err)
	}
	if len(a.Results) != 1 || a.Results[0].SessionID != idA || a.Results[0].Target != targetA || len(a.Results[0].Scores) != 1 || a.ByJudge["judge"].Mean != 1 {
		t.Fatalf("report a = %+v", a)
	}
	// Named by session, and the task the ref named.
	if err := json.Unmarshal(cli(t, "report", "-store", storeB, "-suite", "s", idB), &b); err != nil {
		t.Fatal(err)
	}
	if len(b.Results) != 1 || b.Results[0].SessionID != idB {
		t.Fatalf("report b = %+v", b)
	}
	b.Results[0].Task = a.Results[0].Task
	fileA, fileB := filepath.Join(tmp, "a.json"), filepath.Join(tmp, "b.json")
	for path, r := range map[string]*agenteval.Report{fileA: &a, fileB: &b} {
		data, _ := json.Marshal(r)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var c agenteval.Comparison
	if err := json.Unmarshal(cli(t, "compare", "-a-store", storeA, "-b-store", storeB, fileA, fileB), &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Pairs) != 1 || c.ByJudge["judge"] != -1 || c.Config.Header["dax.workspace"].B != storeB {
		t.Errorf("comparison = %+v", c)
	}
	c = agenteval.Comparison{}
	if err := json.Unmarshal(cli(t, "compare", "-a-store", storeA, "-b-store", storeB, "-ignore-header", "dax.box, dax.workspace ,", fileA, fileB), &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Pairs) != 1 || !c.Config.Empty() {
		t.Errorf("comparison ignoring dax.workspace = %+v", c)
	}

	var doc map[string]any
	if err := json.Unmarshal(cli(t, "export", "-store", storeA, idA+"@"+targetA), &doc); err != nil {
		t.Fatal(err)
	}
	if doc["schema_version"] == nil || doc["steps"] == nil {
		t.Errorf("document = %v", doc)
	}

	// A Harbor job: two trials, each with its store under agent/sessions.
	job := filepath.Join(tmp, "job")
	for trial, reward := range map[string]string{"one__aaa": "1", "two__bbb": "0.5"} {
		record(t, filepath.Join(job, trial, "agent", "sessions"), "", -1)
		data := `{"task_name":"` + strings.Split(trial, "__")[0] + `","verifier_result":{"rewards":{"reward":` + reward + `}}}`
		if err := os.WriteFile(filepath.Join(job, trial, "result.json"), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	reportJob := filepath.Join(tmp, "job.json")
	cli(t, "import", "-out", reportJob, job)
	data, err := os.ReadFile(reportJob)
	if err != nil {
		t.Fatal(err)
	}
	var imported agenteval.Report
	if err := json.Unmarshal(data, &imported); err != nil {
		t.Fatal(err)
	}
	if len(imported.Results) != 2 || imported.ByJudge["harbor/reward"].Count != 2 {
		t.Fatalf("imported = %s", data)
	}
	if err := json.Unmarshal(cli(t, "compare", "-a-job", job, "-b-job", job, reportJob, reportJob), &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Pairs) != 2 || c.Unjudged != 0 || c.ByJudge["harbor/reward"] != 0 {
		t.Errorf("job against itself = %+v", c)
	}
	// Bad command lines exit 2.
	var out, errb bytes.Buffer
	if code := run(ctx, []string{"compare", fileA, fileB}, &out, &errb); code != 2 {
		t.Errorf("compare with no stores: exit %d", code)
	}
	if code := run(ctx, []string{"run"}, &out, &errb); code != 2 {
		t.Errorf("run: exit %d", code)
	}
}

// snapshot appends a snapshot of the run whose last entry is target,
// for task, to the session id in the jsonl store at dir, as a harness
// that runs the agent itself keeps one.
func snapshot(t *testing.T, dir, id, target, task string, data []byte) {
	t.Helper()
	store, err := jsonl.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	e, err := agenteval.NewSnapshot(agenteval.Snapshot{Data: data, Base: "HEAD", Size: int64(len(data))}, target, task)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(context.Background(), id, e); err != nil {
		t.Fatal(err)
	}
}

// checkLeftOut fails unless every result's snapshot in the report r
// kept its size and digest and left its bytes out, counted in Omitted.
func checkLeftOut(t *testing.T, what string, data []byte, n int, results ...*agenteval.Result) {
	t.Helper()
	if bytes.Contains(data, []byte(`"data"`)) {
		t.Errorf("%s holds snapshot bytes:\n%s", what, data)
	}
	if len(results) != n {
		t.Fatalf("%s has %d results with a snapshot, want %d", what, len(results), n)
	}
	for _, res := range results {
		s := res.Snapshot
		if s == nil || s.Data != nil || s.Size == 0 || s.Digest == "" || s.Omitted != s.Size || s.Base != "HEAD" {
			t.Errorf("%s: snapshot = %+v, want Size, Digest and Omitted and no Data", what, s)
		}
	}
}

// TestNoSnapshotData is #81: import, report and compare take
// -no-snapshot-data and write through the library's WriteJSON with
// WithoutSnapshotData, so a report keeps each snapshot's size and digest
// and leaves its bytes out, which the session still holds; compare reads
// the stores again, so two such reports compare as two full ones do.
func TestNoSnapshotData(t *testing.T) {
	tmp := t.TempDir()
	storeA, storeB := filepath.Join(tmp, "a"), filepath.Join(tmp, "b")
	idA, targetA := record(t, storeA, "one", 1)
	idB, targetB := record(t, storeB, "one", 0)
	snapshot(t, storeA, idA, targetA, "one", []byte("diff --git a/x b/x\n+a\n"))
	snapshot(t, storeB, idB, targetB, "one", []byte("diff --git a/x b/x\n+b\n"))
	// Each run named by one ref, which a report takes for the task, so
	// the two reports pair.
	for dir, ref := range map[string]agentsession.RefTarget{storeA: {Session: idA, Entry: targetA}, storeB: {Session: idB, Entry: targetB}} {
		store, err := jsonl.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.UpdateRef(context.Background(), "eval/s/one", agentsession.RefTarget{}, ref, "run"); err != nil {
			t.Fatal(err)
		}
		store.Close()
	}

	reports := map[string]string{}
	for _, side := range []struct{ name, store, id string }{{"a", storeA, "eval/s/one"}, {"b", storeB, "eval/s/one"}} {
		full := filepath.Join(tmp, side.name+"-full.json")
		slim := filepath.Join(tmp, side.name+"-slim.json")
		cli(t, "report", "-store", side.store, "-suite", "s", "-out", full, side.id)
		cli(t, "report", "-store", side.store, "-suite", "s", "-no-snapshot-data", "-out", slim, side.id)
		reports[side.name+"-full"], reports[side.name+"-slim"] = full, slim

		data, err := os.ReadFile(full)
		if err != nil {
			t.Fatal(err)
		}
		var r agenteval.Report
		if err := json.Unmarshal(data, &r); err != nil {
			t.Fatal(err)
		}
		if len(r.Results) != 1 || r.Results[0].Snapshot == nil || r.Results[0].Snapshot.Data == nil {
			t.Fatalf("the full report %s has no snapshot bytes: %s", side.name, data)
		}
		if data, err = os.ReadFile(slim); err != nil {
			t.Fatal(err)
		}
		r = agenteval.Report{}
		if err := json.Unmarshal(data, &r); err != nil {
			t.Fatal(err)
		}
		checkLeftOut(t, "report "+side.name, data, 1, &r.Results[0])
	}

	// The two pairs of reports compare alike: compare reads the stores.
	full := cli(t, "compare", "-a-store", storeA, "-b-store", storeB, reports["a-full"], reports["b-full"])
	slim := cli(t, "compare", "-a-store", storeA, "-b-store", storeB, reports["a-slim"], reports["b-slim"])
	if !bytes.Contains(full, []byte(`"pairs": [
    {`)) {
		t.Fatalf("the reports do not pair:\n%s", full)
	}
	if !bytes.Equal(full, slim) {
		t.Errorf("compare over reports without snapshot bytes =\n%s\nwant as over full ones:\n%s", slim, full)
	}
	out := cli(t, "compare", "-a-store", storeA, "-b-store", storeB, "-no-snapshot-data", reports["a-slim"], reports["b-slim"])
	var c agenteval.Comparison
	if err := json.Unmarshal(out, &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Pairs) != 1 {
		t.Fatalf("comparison = %s", out)
	}
	checkLeftOut(t, "comparison", out, 2, c.Pairs[0].A, c.Pairs[0].B)

	// An imported Harbor job, one trial with a snapshot of its run.
	job := filepath.Join(tmp, "job")
	trial := filepath.Join(job, "one__aaa", "agent", "sessions")
	id, target := record(t, trial, "", -1)
	snapshot(t, trial, id, target, "", []byte("diff --git a/y b/y\n+c\n"))
	if err := os.WriteFile(filepath.Join(job, "one__aaa", "result.json"), []byte(`{"task_name":"one","verifier_result":{"rewards":{"reward":1}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	out = cli(t, "import", "-no-snapshot-data", job)
	var imported agenteval.Report
	if err := json.Unmarshal(out, &imported); err != nil {
		t.Fatal(err)
	}
	if len(imported.Results) != 1 {
		t.Fatalf("imported = %s", out)
	}
	checkLeftOut(t, "import", out, 1, &imported.Results[0])
}
