package harbor_test

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/ChristopherDavenport/agenteval"
	"github.com/ChristopherDavenport/agenteval/harbor"
	"github.com/ChristopherDavenport/agentsession/export"
)

func TestReward(t *testing.T) {
	fsys := os.DirFS("testdata")
	tests := []struct {
		dir  string
		want []agenteval.Score
	}{
		{"trial-json", []agenteval.Score{
			{Judge: "reward", Value: 1, Pass: true},
			{Judge: "speed", Value: 2.5, Pass: true},
			{Judge: "style", Value: 0.5, Pass: false},
		}},
		{"trial-txt", []agenteval.Score{{Judge: "reward", Value: 0, Pass: false}}},
		{"trial-both", []agenteval.Score{{Judge: "reward", Value: 0.25, Pass: false}}},
	}
	for _, tt := range tests {
		t.Run(tt.dir, func(t *testing.T) {
			got, err := harbor.Reward(fsys, tt.dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %d scores, want %d: %+v", len(got), len(tt.want), got)
			}
			for i, w := range tt.want {
				g := got[i]
				if g.Judge != w.Judge || g.Value != w.Value || g.Pass != w.Pass {
					t.Errorf("score %d = %+v, want %+v", i, g, w)
				}
				var details struct {
					File  string  `json:"file"`
					Value float64 `json:"value"`
				}
				if err := json.Unmarshal(g.Details, &details); err != nil || details.Value != w.Value || !strings.Contains(g.Reason, details.File) {
					t.Errorf("score %d details %s reason %q", i, g.Details, g.Reason)
				}
				if tt.dir == "trial-both" && details.File != harbor.RewardJSON {
					t.Errorf("both files: read %s, want the JSON", details.File)
				}
			}
		})
	}
	if _, err := harbor.Reward(fsys, "trial-none"); !errors.Is(err, harbor.ErrNoReward) {
		t.Errorf("no reward: %v", err)
	}
	// The pass rule is the caller's.
	got, err := harbor.Reward(fsys, "trial-json", harbor.WithPass(func(label string, v float64) bool { return label == "style" }))
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Pass || !got[2].Pass {
		t.Errorf("custom pass: %+v", got)
	}
}

func TestRewardErrors(t *testing.T) {
	tests := []struct {
		name string
		fsys fstest.MapFS
		want string
	}{
		{"json not object", fstest.MapFS{"d/reward.json": {Data: []byte("[1]")}}, "reward.json"},
		{"json empty", fstest.MapFS{"d/reward.json": {Data: []byte("{}")}}, "no rewards"},
		{"json not number", fstest.MapFS{"d/reward.json": {Data: []byte(`{"reward":"1"}`)}}, "reward.json"},
		{"text not number", fstest.MapFS{"d/reward.txt": {Data: []byte("one")}}, "not a finite number"},
		{"text infinite", fstest.MapFS{"d/reward.txt": {Data: []byte("inf")}}, "not a finite number"},
		{"text nan", fstest.MapFS{"d/reward.txt": {Data: []byte("NaN")}}, "not a finite number"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := harbor.Reward(tt.fsys, "d"); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestLoad(t *testing.T) {
	task, err := harbor.Load(os.DirFS("testdata"), "task")
	if err != nil {
		t.Fatal(err)
	}
	if task.ID != "hello-miniturn" {
		t.Errorf("id = %q", task.ID)
	}
	if !strings.HasPrefix(task.Instruction, "# Hello") || !strings.HasSuffix(task.Instruction, "`hello`.") {
		t.Errorf("instruction = %q", task.Instruction)
	}
	if len(task.Prompts) != 0 || len(task.Expect) != 0 {
		t.Errorf("task carries prompts or expect: %+v", task)
	}
	wantMeta := map[string]string{
		"task.name":           "hello-miniturn",
		"task.description":    "The smallest task.",
		"task.keywords":       `["smoke","hello"]`,
		"task.authors":        `["a","b"]`,
		"metadata.difficulty": "easy",
		"metadata.category":   "smoke",
	}
	for k, v := range wantMeta {
		if task.Meta[k] != v {
			t.Errorf("meta[%s] = %q, want %q", k, task.Meta[k], v)
		}
	}
	if len(task.Meta) != len(wantMeta) {
		t.Errorf("meta = %v", task.Meta)
	}
	wantSetup := map[string]string{
		"schema_version":             "1.4",
		"multi_step_reward_strategy": "mean",
		"environment.dockerfile":     "environment/Dockerfile",
		"environment.network_mode":   "none",
		"environment.cpus":           "2",
		"environment.memory_mb":      "1024",
		"environment.allowed_hosts":  "[]",
		"environment.mcp_servers":    `[{"name":"fs","transport":"stdio"}]`,
		"agent.timeout_sec":          "300.5",
		"verifier.timeout_sec":       "60",
		"verifier.environment_mode":  "shared",
	}
	for k, v := range wantSetup {
		if task.Setup[k] != v {
			t.Errorf("setup[%s] = %q, want %q", k, task.Setup[k], v)
		}
	}
	if len(task.Setup) != len(wantSetup) {
		t.Errorf("setup = %v", task.Setup)
	}
	// The verifier, the solution and the environment are not loaded.
	if in := task.Inputs(); len(in) != 1 {
		t.Errorf("inputs = %d", len(in))
	}
}

// TestLoadMetaTables is issue 3: [task] and [metadata] are prefixed by
// their own table's name, as every other table already is, so a key
// both tables carry survives in both.
func TestLoadMetaTables(t *testing.T) {
	fsys := fstest.MapFS{
		"t/instruction.md": {Data: []byte("do it\n")},
		"t/task.toml": {Data: []byte(`
[task]
name = "harbor/hello-world"
version = "1.0.0"

[metadata]
name = "the friendly name"
version = "2026-09"
difficulty = "easy"
`)},
	}
	task, err := harbor.Load(fsys, "t")
	if err != nil {
		t.Fatal(err)
	}
	// A real Harbor task name is org/name, so every task ID loaded this
	// way holds a slash.
	if task.ID != "harbor/hello-world" {
		t.Errorf("id = %q", task.ID)
	}
	want := map[string]string{
		"task.name":           "harbor/hello-world",
		"task.version":        "1.0.0",
		"metadata.name":       "the friendly name",
		"metadata.version":    "2026-09",
		"metadata.difficulty": "easy",
	}
	for k, v := range want {
		if task.Meta[k] != v {
			t.Errorf("meta[%s] = %q, want %q", k, task.Meta[k], v)
		}
	}
	if len(task.Meta) != len(want) {
		t.Errorf("the two tables produced %d keys, not the %d they hold: %v", len(task.Meta), len(want), task.Meta)
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name string
		fsys fstest.MapFS
		want string
	}{
		{"no instruction", fstest.MapFS{"t/task.toml": {Data: []byte(`[task]`)}}, "instruction.md"},
		{"empty instruction", fstest.MapFS{"t/instruction.md": {Data: []byte("\n")}}, "is empty"},
		{"bad toml", fstest.MapFS{"t/instruction.md": {Data: []byte("do it")}, "t/task.toml": {Data: []byte("= nope")}}, "decode t/task.toml"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := harbor.Load(tt.fsys, "t"); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want %q", err, tt.want)
			}
		})
	}
	// Without task.toml the task is named after its directory and
	// carries nothing else.
	task, err := harbor.Load(fstest.MapFS{"my-task/instruction.md": {Data: []byte("do it\n")}}, "my-task")
	if err != nil {
		t.Fatal(err)
	}
	if task.ID != "my-task" || task.Instruction != "do it" || task.Meta != nil || task.Setup != nil {
		t.Errorf("task = %+v", task)
	}
	// A task.toml with a name but a directory of another name takes the
	// name; one without a name keeps the directory.
	task, err = harbor.Load(fstest.MapFS{"d/instruction.md": {Data: []byte("x")}, "d/task.toml": {Data: []byte("[task]\ndescription = \"d\"\n")}}, "d")
	if err != nil {
		t.Fatal(err)
	}
	if task.ID != "d" || task.Meta["task.description"] != "d" || task.Setup != nil {
		t.Errorf("task = %+v", task)
	}
}

// verifierJudge is the judge harbor's package comment shows, kept here
// so the example compiles and is exercised: a Harbor task's notion of
// success is a script someone else runs after the agent stops, so the
// judge that fits wraps Reward over the trial's verifier directory
// rather than comparing an expectation against the trajectory.
func verifierJudge(fsys fs.FS, dir string) agenteval.Judge {
	return agenteval.JudgeFunc{
		JudgeName: "harbor",
		Fn: func(context.Context, export.Trajectory, agenteval.Task) (agenteval.Score, error) {
			scores, err := harbor.Reward(fsys, dir)
			if err != nil {
				return agenteval.Score{}, err
			}
			return scores[0], nil
		},
	}
}

func TestVerifierJudge(t *testing.T) {
	tests := []struct {
		dir  string
		pass bool
		err  bool
	}{
		{dir: "trial-json", pass: true},
		{dir: "trial-txt"},
		{dir: "trial-none", err: true},
	}
	for _, tt := range tests {
		t.Run(tt.dir, func(t *testing.T) {
			j := verifierJudge(os.DirFS("testdata"), tt.dir)
			score, err := j.Judge(context.Background(), export.Trajectory{}, agenteval.Task{ID: "harbor/hello-world"})
			if tt.err {
				if !errors.Is(err, harbor.ErrNoReward) {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if score.Judge != "reward" || score.Pass != tt.pass {
				t.Errorf("score = %+v", score)
			}
		})
	}
}
