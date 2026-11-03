package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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
	s, err := store.Create(ctx, agentsession.Header{})
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
	if len(c.Pairs) != 1 || c.ByJudge["judge"] != -1 {
		t.Errorf("comparison = %+v", c)
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
