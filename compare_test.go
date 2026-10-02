package agenteval_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ChristopherDavenport/agenteval"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/compact"
	"github.com/ChristopherDavenport/agentturn/session"
	"github.com/ChristopherDavenport/openresponses"
	"github.com/ChristopherDavenport/openresponses/echo"
)

func TestCompare(t *testing.T) {
	suite := basicSuite(t)
	store := newStableStore()
	a := basicRunner(store)
	b := basicRunner(store)
	// B changes the instructions, drops the tool for every task and
	// adds a passthrough member, so under B the echo adapter never
	// calls upper and answers the prompt itself.
	b.Config = func(task agenteval.Task) agentturn.Config {
		cfg := echoConfig()
		cfg.Instructions = "Be thorough."
		cfg.Tools = nil
		cfg.RequestExtra = map[string]any{"acme_flag": true}
		return cfg
	}
	c, err := agenteval.Compare(context.Background(), suite, a, b)
	if err != nil {
		t.Fatal(err)
	}
	if c.Suite != "basic" || c.A == nil || c.B == nil || len(c.Pairs) != 2 {
		t.Fatalf("comparison = %+v", c)
	}
	greet := c.Pairs[0]
	if greet.Task != "greet" || greet.A.SessionID == greet.B.SessionID {
		t.Errorf("greet pair = %+v", greet)
	}
	// Under A greet passed contains and tool_called; under B neither.
	if greet.Delta["contains"] != -1 || greet.Delta["tool_called:upper"] != -1 || greet.Delta["exact"] != 0 {
		t.Errorf("greet delta = %v", greet.Delta)
	}
	plain := c.Pairs[1]
	if plain.Delta["contains"] != 0 || plain.Delta["exact"] != 0 || plain.Delta["tool_called:upper"] != 0 {
		t.Errorf("plain delta = %v", plain.Delta)
	}
	if c.ByJudge["contains"] != -0.5 || c.ByJudge["tool_called:upper"] != -0.5 || c.ByJudge["exact"] != 0 {
		t.Errorf("by judge = %v", c.ByJudge)
	}
	// The configurations differ the same way for plain, whose A had no
	// tool either, except for the tool: so the pairs' diffs differ and
	// the comparison's is not uniform, while each pair says what
	// changed for it.
	if c.Uniform || !c.Config.Empty() {
		t.Errorf("uniform = %v config = %+v", c.Uniform, c.Config)
	}
	gd := greet.Config
	if gd.Instructions == nil || gd.Instructions.A != "Be brief." || gd.Instructions.B != "Be thorough." {
		t.Errorf("greet instructions diff = %+v", gd.Instructions)
	}
	if strings.Join(gd.ToolsRemoved, ",") != "upper" || len(gd.ToolsAdded) != 0 || len(gd.ToolsChanged) != 0 {
		t.Errorf("greet tools diff = %+v", gd)
	}
	if ch, ok := gd.Extra["acme_flag"]; !ok || ch.A != nil || ch.B != true {
		t.Errorf("greet extra diff = %+v", gd.Extra)
	}
	if gd.Model != nil || gd.Reasoning != nil || gd.Text != nil {
		t.Errorf("greet diff has unexpected members: %+v", gd)
	}
	pd := plain.Config
	if len(pd.ToolsRemoved) != 0 || pd.Instructions == nil || pd.Extra["acme_flag"].B != true {
		t.Errorf("plain diff = %+v", pd)
	}
	var buf bytes.Buffer
	if err := c.WriteJSON(&buf); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(buf.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["pairs"]; !ok {
		t.Errorf("written comparison lacks pairs: %s", buf.String())
	}
	if _, ok := doc["a"]; ok {
		t.Error("written comparison carries the whole report")
	}
}

func TestCompareUniform(t *testing.T) {
	suite := basicSuite(t)
	a := basicRunner(newStableStore())
	b := basicRunner(newStableStore())
	b.Config = func(task agenteval.Task) agentturn.Config {
		cfg := basicConfig(task)
		cfg.ModelName = "echo/echo-2"
		cfg.Reasoning = openresponses.ReasoningConfig{Effort: openresponses.ReasoningEffortHigh}
		return cfg
	}
	c, err := agenteval.Compare(context.Background(), suite, a, b)
	if err != nil {
		t.Fatal(err)
	}
	if !c.Uniform || c.Config.Model == nil || c.Config.Model.B != "echo/echo-2" || c.Config.Reasoning == nil {
		t.Errorf("uniform = %v config = %+v", c.Uniform, c.Config)
	}
	for _, p := range c.Pairs {
		for judge, d := range p.Delta {
			if d != 0 {
				t.Errorf("%s: %s moved by %v", p.Task, judge, d)
			}
		}
	}
	if _, err := agenteval.Compare(context.Background(), suite, a, nil); err == nil {
		t.Error("nil runner accepted")
	}
}

func TestDiffSettings(t *testing.T) {
	tool := func(name, desc string) openresponses.Tool { return openresponses.NewFunctionTool(name, desc, nil) }
	base := agentsession.Settings{Model: "m", Instructions: "i", Tools: openresponses.Tools{tool("a", "A"), tool("b", "B")}, Extra: map[string]json.RawMessage{"k": json.RawMessage(`1`), "gone": json.RawMessage(`"x"`)}}
	tests := []struct {
		name string
		b    agentsession.Settings
		want agenteval.ConfigDiff
	}{
		{"same", base, agenteval.ConfigDiff{}},
		{"model", agentsession.Settings{Model: "n", Instructions: "i", Tools: base.Tools, Extra: base.Extra}, agenteval.ConfigDiff{Model: &agenteval.Change{A: "m", B: "n"}}},
		{"tools", agentsession.Settings{Model: "m", Instructions: "i", Tools: openresponses.Tools{tool("a", "changed"), tool("c", "C")}, Extra: base.Extra},
			agenteval.ConfigDiff{ToolsAdded: []string{"c"}, ToolsRemoved: []string{"b"}, ToolsChanged: []string{"a"}}},
		{"extra", agentsession.Settings{Model: "m", Instructions: "i", Tools: base.Tools, Extra: map[string]json.RawMessage{"k": json.RawMessage(`2`), "new": json.RawMessage(`true`)}},
			agenteval.ConfigDiff{Extra: map[string]agenteval.Change{"k": {A: 1.0, B: 2.0}, "gone": {A: "x"}, "new": {B: true}}}},
		{"text", agentsession.Settings{Model: "m", Instructions: "i", Tools: base.Tools, Extra: base.Extra, Text: openresponses.TextConfig{Verbosity: openresponses.VerbosityLow}},
			agenteval.ConfigDiff{Text: &agenteval.Change{A: openresponses.TextConfig{}, B: openresponses.TextConfig{Verbosity: openresponses.VerbosityLow}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := agenteval.DiffSettings(base, tt.b)
			g, _ := json.Marshal(got)
			w, _ := json.Marshal(tt.want)
			if string(g) != string(w) {
				t.Errorf("diff\n got %s\nwant %s", g, w)
			}
			if got.Empty() != (tt.name == "same") {
				t.Errorf("empty = %v", got.Empty())
			}
		})
	}
}

// TestCompareSeesBeyondTheSettings is one of issue 4's frictions: two
// configurations that differ only in a transform have the same
// settings, and a comparison that can diff settings alone reported
// that nothing about them differed, which is worse than reporting
// nothing.
func TestCompareSeesBeyondTheSettings(t *testing.T) {
	suite := basicSuite(t)
	tests := []struct {
		name   string
		config func(agenteval.Task) agentturn.Config
		want   bool
	}{
		{"the same configuration twice", basicConfig, false},
		{"a context strategy on one side", func(task agenteval.Task) agentturn.Config {
			cfg := basicConfig(task)
			// One note prepended to every call: not a setting, not an
			// item the record holds, and the whole difference between
			// the two runs.
			cfg.Transform = func(_ context.Context, items agentturn.Transcript) (agentturn.Transcript, error) {
				return append(openresponses.Items{openresponses.UserText("Remember: be terse.")}, items...), nil
			}
			return cfg
		}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := basicRunner(newStableStore())
			b := basicRunner(newStableStore())
			b.Config = tt.config
			c, err := agenteval.Compare(context.Background(), suite, a, b)
			if err != nil {
				t.Fatal(err)
			}
			if !c.Uniform {
				t.Fatalf("the pairs differ from each other: %+v", c.Config)
			}
			if c.Config.BeyondSettings != tt.want || c.Config.Empty() == tt.want {
				t.Errorf("beyond_settings = %v, empty = %v, want %v", c.Config.BeyondSettings, c.Config.Empty(), tt.want)
			}
			if c.Config.Instructions != nil || c.Config.Model != nil || len(c.Config.ToolsChanged) != 0 {
				t.Errorf("the settings themselves differ: %+v", c.Config)
			}
			var buf bytes.Buffer
			if err := c.WriteJSON(&buf); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(buf.String(), "beyond_settings") != tt.want {
				t.Errorf("written comparison: %s", buf.String())
			}
		})
	}
}

// TestCompareSeesTheFolds is from issue 20: a comparison of a plain
// configuration against a compacting one says one side folded, which
// the first call alone cannot show.
func TestCompareSeesTheFolds(t *testing.T) {
	prompts := openresponses.Items{openresponses.UserText("one"), openresponses.UserText("two"), openresponses.UserText("three")}
	suite := &agenteval.Suite{Name: "long", Tasks: []agenteval.Task{{ID: "chat", Prompts: prompts}}}
	plain := func(agenteval.Task, *session.Recorder) agentturn.Config { return echoConfig() }
	folding := func(_ agenteval.Task, rec *session.Recorder) agentturn.Config { return foldingConfig(rec) }
	tests := []struct {
		name string
		a, b func(agenteval.Task, *session.Recorder) agentturn.Config
		want *agenteval.Change
	}{
		{"neither folds", plain, plain, nil},
		{"B folds", plain, folding, &agenteval.Change{A: false, B: true}},
		{"both fold", folding, folding, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := &agenteval.Runner{Store: newStableStore(), ConfigWith: tt.a}
			b := &agenteval.Runner{Store: newStableStore(), ConfigWith: tt.b}
			c, err := agenteval.Compare(context.Background(), suite, a, b)
			if err != nil {
				t.Fatal(err)
			}
			got := c.Config.Folded
			if (got == nil) != (tt.want == nil) || (got != nil && *got != *tt.want) {
				t.Errorf("folded = %+v, want %+v", got, tt.want)
			}
			if c.Config.Empty() != (tt.want == nil) {
				t.Errorf("empty = %v with folded = %+v", c.Config.Empty(), got)
			}
		})
	}
}

// verbose answers every summary request at length, so agentturn
// refuses each summary as no smaller than what it folds and every fold
// fails, reporting usage for each call.
type verbose struct{}

func (verbose) CreateStream(_ context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	resp := openresponses.NewResponse(req)
	resp.Usage = &openresponses.Usage{InputTokens: 200, OutputTokens: 400, TotalTokens: 600}
	em := openresponses.NewEmitter(sink, resp)
	if err := em.Item(openresponses.AssistantText(strings.Repeat("a long summary ", 2000))); err != nil {
		return err
	}
	return em.Complete()
}

// TestCompareSeesFailedFolds is #37: a configuration whose every fold
// fails leaves no compaction entry, and still differs from one that
// never compacts, and pays for its summary calls.
func TestCompareSeesFailedFolds(t *testing.T) {
	prompts := openresponses.Items{openresponses.UserText("one"), openresponses.UserText("two"), openresponses.UserText("three"), openresponses.UserText("four")}
	suite := &agenteval.Suite{Name: "long", Tasks: []agenteval.Task{{ID: "chat", Prompts: prompts}}}
	a := &agenteval.Runner{Store: newStableStore(), Cost: flatRate, ConfigWith: func(agenteval.Task, *session.Recorder) agentturn.Config { return echoConfig() }}
	b := &agenteval.Runner{Store: newStableStore(), Cost: flatRate, ConfigWith: func(_ agenteval.Task, rec *session.Recorder) agentturn.Config {
		cfg := echoConfig()
		cfg.Transform = compact.NewLocal(verbose{}, compact.WithBudget(1), compact.WithKeepLast(2), compact.WithMinFold(0), compact.WithOnFold(rec.Fold)).Transform
		return cfg
	}}
	c, err := agenteval.Compare(context.Background(), suite, a, b)
	if err != nil {
		t.Fatal(err)
	}
	if c.Config.Folded != nil {
		t.Fatalf("folded = %+v; the test needs every fold to fail", c.Config.Folded)
	}
	if c.Config.Empty() || c.Config.FoldFailed == nil || c.Config.FoldFailed.A != false || c.Config.FoldFailed.B != true {
		t.Errorf("config = %+v, want B's folds to have failed and A's not", c.Config)
	}
	if p := c.Pairs[0]; p.Config.FoldFailed == nil {
		t.Errorf("the pair's config = %+v", p.Config)
	}
	pa, pb := c.Pairs[0].A, c.Pairs[0].B
	if pb.Usage.OutputTokens < pa.Usage.OutputTokens+400 || pa.CostUSD == nil || pb.CostUSD == nil || *pb.CostUSD <= *pa.CostUSD {
		t.Errorf("A used %+v at %v, B %+v at %v; B's summary calls are not counted", pa.Usage, pa.CostUSD, pb.Usage, pb.CostUSD)
	}
}

// TestCompareFoldsKeepTheSuiteUniform: a compacting side folds on a
// long task and not on a short one, which is the task and not the
// configuration, so the settings both sides differ by for every task
// stay on the comparison.
func TestCompareFoldsKeepTheSuiteUniform(t *testing.T) {
	suite := &agenteval.Suite{Name: "mixed", Tasks: []agenteval.Task{
		{ID: "short", Instruction: "hi"},
		{ID: "long", Prompts: openresponses.Items{openresponses.UserText("one"), openresponses.UserText("two"), openresponses.UserText("three")}},
	}}
	a := &agenteval.Runner{Store: newStableStore(), ConfigWith: func(agenteval.Task, *session.Recorder) agentturn.Config { return echoConfig() }}
	b := &agenteval.Runner{Store: newStableStore(), ConfigWith: func(_ agenteval.Task, rec *session.Recorder) agentturn.Config {
		cfg := echoConfig()
		cfg.Instructions = "Be terse."
		// A budget only the long task's context exceeds.
		cfg.Transform = compact.NewLocal(cfg.Model, compact.WithBudget(150), compact.WithKeepLast(2), compact.WithOnFold(rec.Fold)).Transform
		return cfg
	}}
	c, err := agenteval.Compare(context.Background(), suite, a, b)
	if err != nil {
		t.Fatal(err)
	}
	folds := map[string]bool{}
	for _, p := range c.Pairs {
		folds[p.Task] = p.Config.Folded != nil
	}
	if folds["short"] || !folds["long"] {
		t.Fatalf("the pairs folded %v; the test needs the long task alone to fold", folds)
	}
	if !c.Uniform || c.Config.Instructions == nil {
		t.Errorf("uniform %v, config %+v: the instructions differ for every task", c.Uniform, c.Config)
	}
	if c.Config.Folded == nil || c.Config.Folded.A != false || c.Config.Folded.B != true {
		t.Errorf("folded = %+v, want B's runs to have folded and A's not", c.Config.Folded)
	}
}

// failAfter answers through the echo adapter n times and then refuses
// every call with a 400, the error an inference server returns for a
// context it cannot take, which no retry policy retries.
type failAfter struct {
	n     int
	calls atomic.Int32
	echo  echo.Adapter
}

func (m *failAfter) CreateStream(ctx context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	if int(m.calls.Add(1)) > m.n {
		return &openresponses.Error{Type: openresponses.ErrorTypeInvalidRequest, StatusCode: 400, Message: "context length exceeded"}
	}
	return m.echo.CreateStream(ctx, req, sink)
}

// TestCompareSeesUnjudgedRuns is #44: under the default of leaving a
// failed run unjudged, a comparison where B crashed on a task said
// nothing about it, and B's summary read as good as A's over fewer
// tasks. The summary now counts what it left out and the comparison
// marks the pair.
func TestCompareSeesUnjudgedRuns(t *testing.T) {
	suite := basicSuite(t)
	for _, judgeFailed := range []bool{false, true} {
		t.Run(fmt.Sprintf("JudgeFailedRuns %v", judgeFailed), func(t *testing.T) {
			a, b := basicRunner(newStableStore()), basicRunner(newStableStore())
			a.JudgeFailedRuns, b.JudgeFailedRuns = judgeFailed, judgeFailed
			// B answers plain's first prompt and fails its second.
			b.Config = func(task agenteval.Task) agentturn.Config {
				cfg := basicConfig(task)
				if task.ID == "plain" {
					cfg.Model = &failAfter{n: 1}
				}
				return cfg
			}
			c, err := agenteval.Compare(context.Background(), suite, a, b)
			if err != nil {
				t.Fatal(err)
			}
			if len(c.Pairs) != 2 {
				t.Fatalf("%d pairs", len(c.Pairs))
			}
			greet, plain := c.Pairs[0], c.Pairs[1]
			if plain.B.Err == nil || plain.B.Reason != agentturn.ReasonError {
				t.Fatalf("B's plain run did not fail: reason %s, err %v", plain.B.Reason, plain.B.Err)
			}
			for _, name := range c.A.Judges() {
				if sum := c.A.ByJudge[name]; sum.Count != 2 || sum.Unjudged != 0 {
					t.Errorf("A %s: %+v, want both tasks judged", name, sum)
				}
			}
			if judgeFailed {
				// A crash counts as a failure: the failed run scores 0
				// on every judge, and the comparison sees the drop.
				if plain.Unjudged != "" || greet.Unjudged != "" || c.Unjudged != 0 {
					t.Errorf("unjudged: greet %q, plain %q, comparison %d; want none", greet.Unjudged, plain.Unjudged, c.Unjudged)
				}
				if sum := c.B.ByJudge["exact"]; sum.Count != 2 || sum.Unjudged != 0 || sum.Mean != 0 {
					t.Errorf("B exact: %+v, want the failed run scored 0", sum)
				}
				if plain.Delta["exact"] != -1 || c.ByJudge["exact"] != -0.5 {
					t.Errorf("plain delta %v, by judge %v; want exact down by one", plain.Delta, c.ByJudge)
				}
				return
			}
			if plain.Unjudged != agenteval.UnjudgedB || greet.Unjudged != "" || c.Unjudged != 1 {
				t.Errorf("unjudged: greet %q, plain %q, comparison %d; want plain's B side", greet.Unjudged, plain.Unjudged, c.Unjudged)
			}
			if len(plain.Delta) != 0 || len(plain.B.Scores) != 0 {
				t.Errorf("plain delta %v, B scores %+v; want none for an unjudged side", plain.Delta, plain.B.Scores)
			}
			for _, name := range c.B.Judges() {
				if sum := c.B.ByJudge[name]; sum.Count != 1 || sum.Unjudged != 1 {
					t.Errorf("B %s: %+v, want one judged and one left out", name, sum)
				}
			}
			// The count survives the report being written and read back.
			var buf bytes.Buffer
			if err := c.B.WriteJSON(&buf); err != nil {
				t.Fatal(err)
			}
			back, err := agenteval.ReadReport(&buf)
			if err != nil {
				t.Fatal(err)
			}
			if sum := agenteval.Summarize(back.Results)["exact"]; sum.Count != 1 || sum.Unjudged != 1 {
				t.Errorf("summarised from the read-back report: %+v", sum)
			}
			if c.ByJudge["exact"] != 0 {
				t.Errorf("by judge %v: an unjudged pair is left out of the mean", c.ByJudge)
			}
		})
	}
}
