package agenteval_test

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agenteval"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
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
