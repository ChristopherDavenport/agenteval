package harbor_test

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/ChristopherDavenport/agenteval"
	"github.com/ChristopherDavenport/agenteval/harbor"
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
		"name":        "hello-miniturn",
		"description": "The smallest task.",
		"keywords":    `["smoke","hello"]`,
		"authors":     `["a","b"]`,
		"difficulty":  "easy",
		"category":    "smoke",
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
	if task.ID != "d" || task.Meta["description"] != "d" || task.Setup != nil {
		t.Errorf("task = %+v", task)
	}
}
