package judge_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agenteval"
	"github.com/ChristopherDavenport/agenteval/judge"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/export"
)

func loadFixture(t *testing.T, name string) *agentsession.Session {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "testdata", "sessions", name+".jsonl"))
	if err != nil {
		t.Fatalf("%v (run go test . -update at the root to create it)", err)
	}
	defer f.Close()
	s, err := agentsession.Read(f)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// mainTrajectory returns the session's main trajectory.
func mainTrajectory(t *testing.T, s *agentsession.Session) export.Trajectory {
	t.Helper()
	for tr, err := range export.Trajectories(s) {
		if err != nil {
			t.Fatal(err)
		}
		if tr.Main {
			return tr
		}
	}
	t.Fatal("no main trajectory")
	return export.Trajectory{}
}

func TestDeterministicJudges(t *testing.T) {
	basic := mainTrajectory(t, loadFixture(t, "basic"))
	tests := []struct {
		name    string
		judge   agenteval.Judge
		expect  string
		want    string // judge name
		pass    bool
		reason  string
		wantErr bool
	}{
		{name: "exact whole", judge: judge.Exact(""), expect: `"Tool result: HELLO WORLD"`, want: "exact", pass: true},
		{name: "exact field", judge: judge.Exact("text"), expect: `{"text":"Tool result: HELLO WORLD"}`, want: "exact", pass: true},
		{name: "exact differs", judge: judge.Exact(""), expect: `"other"`, want: "exact", pass: false, reason: `is not "other"`},
		{name: "exact missing", judge: judge.Exact("text"), expect: `{"contains":["x"]}`, want: "exact", pass: false, reason: "no expected text"},
		{name: "exact no expect", judge: judge.Exact(""), want: "exact", pass: false, reason: "no expected text"},
		{name: "exact bad expect", judge: judge.Exact("text"), expect: `"not an object"`, wantErr: true},
		{name: "contains one", judge: judge.Contains(""), expect: `"HELLO"`, want: "contains", pass: true},
		{name: "contains many", judge: judge.Contains("contains"), expect: `{"contains":["Tool","HELLO WORLD"]}`, want: "contains", pass: true},
		{name: "contains lacks", judge: judge.Contains("contains"), expect: `{"contains":["HELLO","planet"]}`, want: "contains", pass: false, reason: `lacks ["planet"]`},
		{name: "contains bad shape", judge: judge.Contains(""), expect: `{"a":1}`, wantErr: true},
		{name: "tool called", judge: judge.ToolCalled("upper"), want: "tool_called:upper", pass: true, reason: "called 1 time"},
		{name: "tool not called", judge: judge.ToolCalled("lower"), want: "tool_called:lower", pass: false, reason: "not called"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			task := agenteval.Task{ID: "t", Expect: json.RawMessage(tt.expect)}
			if tt.expect == "" {
				task.Expect = nil
			}
			score, err := tt.judge.Judge(context.Background(), basic, task)
			if tt.wantErr {
				if err == nil {
					t.Fatal("no error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if score.Judge != tt.want || tt.judge.Name() != tt.want {
				t.Errorf("judge = %q / %q, want %q", score.Judge, tt.judge.Name(), tt.want)
			}
			if score.Pass != tt.pass {
				t.Errorf("pass = %v, want %v (%s)", score.Pass, tt.pass, score.Reason)
			}
			if want := 0.0; tt.pass {
				want = 1
				if score.Value != want {
					t.Errorf("value = %v, want %v", score.Value, want)
				}
			} else if score.Value != want {
				t.Errorf("value = %v, want %v", score.Value, want)
			}
			if tt.reason != "" && !strings.Contains(score.Reason, tt.reason) {
				t.Errorf("reason %q lacks %q", score.Reason, tt.reason)
			}
		})
	}
}

func TestFinalTextAndCalls(t *testing.T) {
	multi := mainTrajectory(t, loadFixture(t, "multi"))
	if got := judge.FinalText(multi); got != "second" {
		t.Errorf("final text = %q", got)
	}
	if calls := judge.Calls(multi); len(calls) != 0 {
		t.Errorf("calls = %d", len(calls))
	}
	basic := mainTrajectory(t, loadFixture(t, "basic"))
	if calls := judge.Calls(basic); len(calls) != 1 || calls[0].Name != "upper" {
		t.Errorf("calls = %+v", calls)
	}
	if got := judge.FinalText(export.Trajectory{}); got != "" {
		t.Errorf("empty trajectory text = %q", got)
	}
}

func judgeTask(id string) agenteval.Task { return agenteval.Task{ID: id} }
