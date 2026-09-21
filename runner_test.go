package agenteval_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agenteval"
	"github.com/ChristopherDavenport/agenteval/judge"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/export"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// basicSuite loads testdata/suites/basic.
func basicSuite(t *testing.T) *agenteval.Suite {
	t.Helper()
	suite, err := agenteval.LoadSuite(os.DirFS("testdata/suites"), "basic")
	if err != nil {
		t.Fatal(err)
	}
	return suite
}

// basicConfig is the configuration under test: the echo configuration,
// without tools for a task whose setup says so.
func basicConfig(task agenteval.Task) agentturn.Config {
	cfg := echoConfig()
	if task.Setup["tools"] == "none" {
		cfg.Tools = nil
	}
	return cfg
}

// flatRate prices every call at one dollar per token in and two out,
// whole numbers so the golden carries no float noise.
func flatRate(_ string, u openresponses.Usage) (float64, bool) {
	return float64(u.InputTokens) + 2*float64(u.OutputTokens), true
}

func basicRunner(store agentsession.Store) *agenteval.Runner {
	return &agenteval.Runner{
		Store:  store,
		Config: basicConfig,
		Judges: []agenteval.Judge{judge.Contains("contains"), judge.Exact("text"), judge.ToolCalled("upper")},
		Header: func(task agenteval.Task) agentsession.Header {
			return agentsession.Header{Harness: &agentsession.Harness{Name: "agenteval-test", Version: "1"}, CWD: "/work/" + task.ID}
		},
		Cost: flatRate,
	}
}

func checkGolden(t *testing.T, path string, got []byte) {
	t.Helper()
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden: %v (run with -update to create it)", err)
	}
	if !bytes.Equal(want, got) {
		t.Errorf("golden %s differs\nwant:\n%s\ngot:\n%s", path, want, got)
	}
}

func TestRunnerReport(t *testing.T) {
	store := newStableStore()
	r := basicRunner(store)
	report, err := r.Run(context.Background(), basicSuite(t))
	if err != nil {
		t.Fatal(err)
	}
	if report.Suite != "basic" || len(report.Results) != 2 {
		t.Fatalf("report = %+v", report)
	}
	greet, plain := report.Results[0], report.Results[1]
	for _, res := range report.Results {
		if res.Err != nil {
			t.Errorf("%s: %v", res.Task.ID, res.Err)
		}
		if res.Reason != agentturn.ReasonDone {
			t.Errorf("%s: reason %s", res.Task.ID, res.Reason)
		}
		if len(res.Scores) != 3 || len(res.Ends) != res.Runs {
			t.Errorf("%s: %d scores, %d ends, %d runs", res.Task.ID, len(res.Scores), len(res.Ends), res.Runs)
		}
		if res.CostUSD == nil || *res.CostUSD <= 0 || res.Usage.TotalTokens == 0 {
			t.Errorf("%s: cost %v usage %+v", res.Task.ID, res.CostUSD, res.Usage)
		}
	}
	if greet.Runs != 1 || plain.Runs != 2 {
		t.Errorf("runs: greet %d plain %d", greet.Runs, plain.Runs)
	}
	pass := func(res agenteval.Result) string {
		var out []string
		for _, s := range res.Scores {
			out = append(out, s.Judge+"="+map[bool]string{true: "pass", false: "fail"}[s.Pass])
		}
		return strings.Join(out, " ")
	}
	if got := pass(greet); got != "contains=pass exact=fail tool_called:upper=pass" {
		t.Errorf("greet: %s", got)
	}
	if got := pass(plain); got != "contains=fail exact=pass tool_called:upper=fail" {
		t.Errorf("plain: %s", got)
	}
	if got := report.Judges(); strings.Join(got, ",") != "contains,exact,tool_called:upper" {
		t.Errorf("judges = %v", got)
	}
	for _, name := range report.Judges() {
		sum := report.ByJudge[name]
		if sum.Count != 2 || sum.Mean != 0.5 || sum.Passed != 1 || sum.PassRate != 0.5 {
			t.Errorf("%s: %+v", name, sum)
		}
	}
	if res, ok := report.Result("plain"); !ok || res.Task.ID != "plain" {
		t.Error("Result(plain) not found")
	}
	if _, ok := report.Result("nope"); ok {
		t.Error("Result(nope) found")
	}

	// The report is the golden, and reads back to the same value.
	var buf bytes.Buffer
	if err := report.WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}
	checkGolden(t, filepath.Join("testdata", "report", "basic.json"), buf.Bytes())
	back, err := agenteval.ReadReport(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	var again bytes.Buffer
	if err := back.WriteJSON(&again); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf.Bytes(), again.Bytes()) {
		t.Errorf("report does not round-trip:\n%s\n%s", buf.Bytes(), again.Bytes())
	}
}

func TestRunnerSessions(t *testing.T) {
	store := newStableStore()
	r := basicRunner(store)
	suite := basicSuite(t)
	report, err := r.Run(context.Background(), suite)
	if err != nil {
		t.Fatal(err)
	}
	greet := report.Results[0]
	s, err := store.Open(context.Background(), greet.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if h := s.Header(); h.CWD != "/work/greet" || h.Harness == nil || h.Harness.Name != "agenteval-test" {
		t.Errorf("header = %+v", h)
	}
	want := "info env custom run config item:user item:function_call* response dispatch item:function_call_output item:assistant* response run outcome outcome outcome"
	if got := entryTypes(s); got != want {
		t.Fatalf("entries = %q\nwant      %q", got, want)
	}
	if s.Name() != "greet" {
		t.Errorf("name = %q", s.Name())
	}
	entries := s.Entries()
	env := entries[1].(*agentsession.EnvEntry)
	if env.Files == nil || len(env.Files.Read) != len(suite.Manifest.Files) || env.Files.Read["basic/greet.json"] != suite.Manifest.Files["basic/greet.json"] {
		t.Errorf("env = %+v", env)
	}
	custom := entries[2].(*agentsession.CustomEntry)
	var rec agenteval.TaskRecord
	if custom.NS != agenteval.TaskNS || json.Unmarshal(custom.Data, &rec) != nil || rec.Suite != "basic" || rec.Task != "greet" || rec.Location != "basic" || rec.Meta["kind"] != "tool" {
		t.Errorf("custom = %s %s", custom.NS, custom.Data)
	}
	// Every response verifies: the describing entries did not disturb
	// the recorded path.
	if n := verifyAll(t, s); n != 2 {
		t.Errorf("responses = %d", n)
	}
	// The outcomes target the run's last entry, its end record, which
	// every score of the result targets, and carry the RFC's members.
	runEnd := entries[12].(*agentsession.RunEntry)
	if greet.Target != runEnd.ID {
		t.Errorf("target = %s, want %s", greet.Target, runEnd.ID)
	}
	for i, e := range entries[13:] {
		o := e.(*agentsession.OutcomeEntry)
		score := greet.Scores[i]
		if o.Kind != agentsession.OutcomeEval || o.Target != greet.Target || o.Label != score.Judge || o.Score == nil || *o.Score != score.Value {
			t.Errorf("outcome %d = %+v", i, o)
		}
		got, details, ok := agenteval.ReadOutcome(o)
		if !ok || got.Judge != score.Judge || got.Value != score.Value || got.Pass != score.Pass || got.Reason != score.Reason || details.Task != "greet" {
			t.Errorf("read outcome %d = %+v %+v", i, got, details)
		}
		line, err := agentsession.MarshalEntry(o)
		if err != nil {
			t.Fatal(err)
		}
		if want := `"pass":` + map[bool]string{true: "true", false: "false"}[score.Pass]; !strings.Contains(string(line), want) {
			t.Errorf("outcome %d line lacks %s: %s", i, want, line)
		}
		if strings.Contains(string(line), `"target":"greet"`) {
			t.Errorf("outcome %d targets the task ID: %s", i, line)
		}
	}
	// Written and read back, the outcomes still carry pass, and the
	// export's preference can resolve their target.
	var buf bytes.Buffer
	if err := agentsession.Write(&buf, s); err != nil {
		t.Fatal(err)
	}
	back, err := agentsession.Read(&buf)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, e := range back.Entries() {
		if o, ok := e.(*agentsession.OutcomeEntry); ok {
			score, details, ok := agenteval.ReadOutcome(o)
			if !ok || details.Task != "greet" {
				t.Errorf("read back: %+v", o)
			}
			if score.Judge == "contains" && !score.Pass {
				t.Error("read back: contains lost its pass")
			}
			found++
		}
	}
	if found != 3 {
		t.Errorf("read back %d outcomes", found)
	}
	// The exported document lifts the manifest and carries every
	// outcome in its final metrics. None reaches a step: the export
	// indexes items and responses to steps, not the run end the
	// outcomes target, so the step-level list waits on the export
	// resolving a record to the step that closes its segment.
	for tr, err := range export.Trajectories(back) {
		if err != nil {
			t.Fatal(err)
		}
		if !tr.Main {
			continue
		}
		doc, err := export.ToATIF(tr, export.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if doc.Extra[export.ExtraEnvironment] == nil {
			t.Error("document has no environment")
		}
		if doc.FinalMetrics == nil {
			t.Fatal("document has no final metrics")
		}
		if list, _ := doc.FinalMetrics.Extra[export.ExtraOutcome].([]any); len(list) != 3 {
			t.Errorf("document carries %d outcomes: %v", len(list), doc.FinalMetrics.Extra)
		}
	}
}

func TestRunnerParallelKeepsOrder(t *testing.T) {
	sequential := basicRunner(newStableStore())
	parallel := basicRunner(newStableStore())
	parallel.Parallel = 4
	a, err := sequential.Run(context.Background(), basicSuite(t))
	if err != nil {
		t.Fatal(err)
	}
	b, err := parallel.Run(context.Background(), basicSuite(t))
	if err != nil {
		t.Fatal(err)
	}
	var ba, bb bytes.Buffer
	if err := a.WriteJSON(&ba); err != nil {
		t.Fatal(err)
	}
	if err := b.WriteJSON(&bb); err != nil {
		t.Fatal(err)
	}
	// Session IDs are assigned in start order, which parallel runs may
	// swap; everything else is the same.
	norm := func(s string) string {
		return strings.ReplaceAll(strings.ReplaceAll(s, "000000000001", "N"), "000000000002", "N")
	}
	if norm(ba.String()) != norm(bb.String()) {
		t.Errorf("parallel report differs:\n%s\n%s", ba.String(), bb.String())
	}
}

func TestRunnerErrors(t *testing.T) {
	ctx := context.Background()
	suite := basicSuite(t)
	if _, err := (&agenteval.Runner{Config: basicConfig}).Run(ctx, suite); err == nil || !strings.Contains(err.Error(), "no store") {
		t.Errorf("no store: %v", err)
	}
	if _, err := (&agenteval.Runner{Store: newStableStore()}).Run(ctx, suite); err == nil || !strings.Contains(err.Error(), "no config") {
		t.Errorf("no config: %v", err)
	}
	if _, err := basicRunner(newStableStore()).Run(ctx, &agenteval.Suite{}); err == nil || !strings.Contains(err.Error(), "no tasks") {
		t.Errorf("no tasks: %v", err)
	}

	// A judge that fails leaves the other scores in place and names
	// itself on the result; a task with nothing to send is an error of
	// its own.
	r := basicRunner(newStableStore())
	boom := errors.New("boom")
	r.Judges = append(r.Judges, agenteval.JudgeFunc{JudgeName: "broken", Fn: func(context.Context, export.Trajectory, agenteval.Task) (agenteval.Score, error) {
		return agenteval.Score{}, boom
	}})
	report, err := r.Run(ctx, &agenteval.Suite{Name: "s", Tasks: []agenteval.Task{suite.Tasks[0], {ID: "empty"}}})
	if err != nil {
		t.Fatal(err)
	}
	greet, empty := report.Results[0], report.Results[1]
	if !errors.Is(greet.Err, boom) || len(greet.Scores) != 3 {
		t.Errorf("greet: err %v, %d scores", greet.Err, len(greet.Scores))
	}
	if empty.Err == nil || !strings.Contains(empty.Err.Error(), "nothing to send") || empty.SessionID != "" {
		t.Errorf("empty: %+v", empty)
	}
	if _, ok := report.ByJudge["broken"]; ok {
		t.Error("a judge that never scored is summarised")
	}
	var buf bytes.Buffer
	if err := report.WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"error": "agenteval: task greet: judge broken: boom"`) {
		t.Errorf("report lacks the error: %s", buf.String())
	}
	back, err := agenteval.ReadReport(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if back.Results[0].Err == nil || back.Results[0].Err.Error() != greet.Err.Error() {
		t.Errorf("read back error = %v", back.Results[0].Err)
	}

	// A run that ends input_required stops the task there: the second
	// prompt is not sent because it cannot answer the pending call.
	r = basicRunner(newStableStore())
	r.Config = func(task agenteval.Task) agentturn.Config {
		cfg := echoConfig()
		cfg.BeforeToolCall = func(context.Context, agentturn.ToolCallInfo) (*agentturn.ToolDecision, error) {
			return &agentturn.ToolDecision{Action: agentturn.Defer}, nil
		}
		return cfg
	}
	report, err = r.Run(ctx, &agenteval.Suite{Name: "s", Tasks: []agenteval.Task{{ID: "two", Prompts: openresponses.Items{openresponses.UserText("a"), openresponses.UserText("b")}}}})
	if err != nil {
		t.Fatal(err)
	}
	two := report.Results[0]
	if two.Reason != agentturn.ReasonInputRequired || two.Runs != 1 || two.Err != nil || len(two.Scores) != 3 {
		t.Errorf("two = reason %s runs %d err %v scores %d", two.Reason, two.Runs, two.Err, len(two.Scores))
	}

	// A model that fails ends the task with the error and no scores
	// beyond what the judges can still say about the recorded path.
	r = basicRunner(newStableStore())
	r.Config = func(agenteval.Task) agentturn.Config {
		return agentturn.Config{Model: failingModel{}}
	}
	report, err = r.Run(ctx, suite)
	if err != nil {
		t.Fatal(err)
	}
	if res := report.Results[0]; res.Reason != agentturn.ReasonError || res.Err == nil || !strings.Contains(res.Err.Error(), "run 1") {
		t.Errorf("failing model: %+v", res)
	}

	// A cancelled context stops the suite; the report holds what ran.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	report, err = basicRunner(newStableStore()).Run(cancelled, suite)
	if !errors.Is(err, context.Canceled) || report == nil || len(report.Results) != 2 {
		t.Errorf("cancelled: %v %+v", err, report)
	}
	for _, res := range report.Results {
		if !errors.Is(res.Err, context.Canceled) {
			t.Errorf("%s: %v", res.Task.ID, res.Err)
		}
	}
}

// failingModel fails every call.
type failingModel struct{}

func (failingModel) CreateStream(context.Context, openresponses.Request, openresponses.EventSink) error {
	return errors.New("model down")
}
