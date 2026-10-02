package replay_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agenteval/replay"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/compact"
	"github.com/ChristopherDavenport/openresponses"
)

// The fixtures notext.jsonl and restart.jsonl under testdata/sessions
// were recorded by the round 7 tags, agentturn/session v0.0.14 and
// agentsession v0.0.18, by the harbor-eval study's gen018 generator
// (examples/round8/harbor-eval/probe/testdata/v0.0.14), and are copied
// here as written: go test . -update does not regenerate them. Each is
// a terminal agent over the echo adapter with one bash tool, folding
// through compact.NewLocal; what each holds is in its test case.

type bashArgs struct {
	Command string `json:"command" desc:"The shell command to run"`
}

// terminalConfig is the configuration gen018 recorded the fixtures
// under, with model behind it: the tool's name is what dispatch needs,
// since BeforeModelCall serves the recorded tool list and instructions.
func terminalConfig(model agentturn.Model) agentturn.Config {
	bash := agenttool.New("bash", "Run a shell command", func(_ context.Context, a bashArgs) (string, error) {
		return "<returncode>0</returncode>\n<output>" + strings.ToUpper(a.Command) + "</output>", nil
	}, agenttool.WithSequential())
	return agentturn.Config{
		Name:         "miniturn",
		Model:        model,
		ModelName:    "echo/echo-1",
		Instructions: "You are a terminal agent. Run one command per step.",
		Tools:        []agenttool.Tool{bash},
		MaxTurns:     30,
	}
}

// Issue 49: a divergence at a fold the replay did not make names its
// cause. After a fold that failed on a summary with no text, which
// failed the recording's turn before agentturn v0.0.15, the replay
// sends the turn on unfolded; at a fold the recording asked for again
// after a restart, the replay's transform backs off. Neither is the
// minimum fold, so neither names compact.WithMinFold(0), which the
// replay here already passes.
func TestASkippedFoldNamesItsCause(t *testing.T) {
	longPrompt := strings.Repeat("Here is the log I want you to read. ", 50)
	tests := []struct {
		name     string
		budget   int
		keepLast int
		prompts  []string
		// served is the step the replay diverges at, and want and
		// unwanted are what the divergence says and does not.
		served   int
		want     []string
		unwanted []string
	}{
		// v0.0.14 asked a summary with no text twice, recorded one
		// compaction_failed with two attempts and failed the turn; the
		// next prompt folded.
		{"notext", 60, 2, []string{"list the files", "what is in notes.txt", "count the lines", "say done"}, 4,
			[]string{"no text", "unfolded", "does not replay strictly past"}, []string{"WithMinFold"}},
		// v0.0.14's fold of the first prompt failed as too large; the
		// host restarted and asked again about the same prefix, which a
		// transform in one process backs off from.
		{"restart", 400, 4, []string{longPrompt, "two", "three", "four", "five", "six"}, 6,
			[]string{"asked again", "session.CompactOptions", "backs off", "agentturn#211"}, []string{"WithMinFold"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := loadFixture(t, tt.name)
			model, err := replay.NewModel(s, replay.Strict())
			if err != nil {
				t.Fatal(err)
			}
			cfg := terminalConfig(model)
			cfg.BeforeModelCall = model.BeforeModelCall
			cfg.Transform = compact.NewLocal(model, compact.WithBudget(tt.budget), compact.WithKeepLast(tt.keepLast), compact.WithMinFold(0)).Transform
			a := agentturn.New(cfg)
			for _, p := range tt.prompts {
				if _, err = a.Prompt(context.Background(), openresponses.UserText(p)); err != nil {
					break
				}
			}
			if !errors.Is(err, replay.ErrDiverged) || model.Served() != tt.served {
				t.Fatalf("served %d of %d steps, err = %v; want a divergence at step %d", model.Served(), model.Steps(), err, tt.served)
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("the divergence does not say %q:\n%v", w, err)
				}
			}
			for _, u := range tt.unwanted {
				if strings.Contains(err.Error(), u) {
					t.Errorf("the divergence names %q, which does not apply:\n%v", u, err)
				}
			}
		})
	}
}
