package replay_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agenteval/replay"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/compact"
	"github.com/ChristopherDavenport/agentturn/session"
	"github.com/ChristopherDavenport/openresponses"
)

// shortSummary answers a fold's summary call with a summary small
// enough to apply.
func shortSummary(req openresponses.Request, sink openresponses.EventSink) error {
	em := openresponses.NewEmitter(sink, openresponses.NewResponse(req))
	if err := em.Item(openresponses.AssistantText("short")); err != nil {
		return err
	}
	return em.Complete()
}

// localFold folds through model at every turn, with opts. The budget
// is the smallest for which compact.NewLocal sets max_output_tokens,
// half of it.
func localFold(model openresponses.Streamer, opts ...compact.Option) func(*agentturn.Config, *session.Recorder) {
	return func(cfg *agentturn.Config, rec *session.Recorder) {
		opts := append(opts, compact.WithBudget(2), compact.WithKeepLast(2), compact.WithOnFold(rec.Fold))
		cfg.Transform = compact.NewLocal(model, opts...).Transform
	}
}

// noOutputLimit sends a fold's request as agentturn v0.0.12 did, with
// no max_output_tokens.
var noOutputLimit = compact.WithRequest(func(req *openresponses.Request) { req.MaxOutputTokens = nil })

// Issue 38: a fold recorded before agentturn v0.0.13 sent no
// max_output_tokens. A strict replay under v0.0.13 diverges at it and
// says the limit is the only difference, and where to clear it; a fold
// that differs otherwise says nothing of the kind.
func TestAFoldThatDiffersOnlyInItsOutputLimit(t *testing.T) {
	live := summarizer{summary: shortSummary}
	orig, _, err := rerunWith(t, fixtureConfig(live), localFold(live, noOutputLimit), "one", "two")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		opts []compact.Option
		// diverges is whether the replay diverges at the fold, and
		// limit whether it says the output limit is why.
		diverges, limit bool
	}{
		{"as configured", nil, true, true},
		{"another prompt", []compact.Option{compact.WithSummaryPrompt("Write a haiku.")}, true, false},
		{"limit cleared", []compact.Option{noOutputLimit}, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model, err := replay.NewModel(orig, replay.Strict())
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = rerunWith(t, fixtureConfig(model), localFold(model, tt.opts...), "one", "two")
			if !tt.diverges {
				if err != nil {
					t.Fatalf("strict replay: %v", err)
				}
				return
			}
			if !errors.Is(err, replay.ErrDiverged) || !strings.Contains(err.Error(), "the fold's request") {
				t.Fatalf("err = %v, want a divergence at the fold", err)
			}
			if said := strings.Contains(err.Error(), "max_output_tokens") && strings.Contains(err.Error(), "compact.WithRequest"); said != tt.limit {
				t.Errorf("err = %v; names the output limit %v, want %v", err, said, tt.limit)
			}
		})
	}
}

// Issue 38: a fold agentturn v0.0.12 applied with a summary no smaller
// than what it folded is served, refused, and asked again; the second
// summary call reaches the next step, and the divergence says so.
func TestAFoldTheLoopRefused(t *testing.T) {
	live := summarizer{summary: shortSummary}
	orig, _, err := rerunWith(t, fixtureConfig(live), localFold(live), "one", "two")
	if err != nil {
		t.Fatal(err)
	}
	// What v0.0.12 would have applied: a summary larger than anything
	// it folds.
	applied := 0
	for _, e := range orig.Path(orig.Leaf()) {
		if c, ok := e.(*agentsession.CompactionEntry); ok {
			c.Summary = compact.SummaryMessage(strings.Repeat("a long summary ", 2000))
			applied++
		}
	}
	if applied == 0 {
		t.Fatal("the recording holds no fold")
	}
	model, err := replay.NewModel(orig, replay.Strict())
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = rerunWith(t, fixtureConfig(model), localFold(model), "one", "two")
	if !errors.Is(err, replay.ErrDiverged) || !strings.Contains(err.Error(), "the request of the fold served at step") {
		t.Fatalf("err = %v, want a divergence naming the fold served before it", err)
	}
}

// Issue 38: agentturn v0.0.12 recorded no attempt count, and asked a
// summary with no text twice before the fold failed. Such a record is
// served as two calls, and the replay goes on as recorded.
func TestANoTextFoldBeforeAttempts(t *testing.T) {
	live := summarizer{summary: func(req openresponses.Request, sink openresponses.EventSink) error {
		return emptyResponse(req, sink, "")
	}}
	orig, _, origErr := rerunWith(t, fixtureConfig(live), localFold(live), "one", "two")
	if origErr == nil {
		t.Fatal("the recording did not fail")
	}
	before, err := replay.NewModel(orig, replay.Strict())
	if err != nil {
		t.Fatal(err)
	}
	stripped := 0
	for _, e := range orig.Path(orig.Leaf()) {
		c, ok := e.(*agentsession.CustomEntry)
		if !ok || c.NS != session.FailedFoldNS {
			continue
		}
		var data map[string]any
		if err := json.Unmarshal(c.Data, &data); err != nil {
			t.Fatal(err)
		}
		if data["attempts"] != float64(2) {
			t.Fatalf("the recording's fold made %v calls, the test needs 2", data["attempts"])
		}
		delete(data, "attempts")
		if c.Data, err = json.Marshal(data); err != nil {
			t.Fatal(err)
		}
		stripped++
	}
	if stripped == 0 {
		t.Fatal("the recording holds no failed fold")
	}
	model, err := replay.NewModel(orig, replay.Strict())
	if err != nil {
		t.Fatal(err)
	}
	if model.Steps() != before.Steps() {
		t.Errorf("%d steps without the attempt count, %d with it", model.Steps(), before.Steps())
	}
	_, _, err = rerunWith(t, fixtureConfig(model), localFold(model), "one", "two")
	if err == nil || unserved(err.Error()) != origErr.Error() {
		t.Fatalf("strict replay: err = %v, want %v", err, origErr)
	}
	if model.Served() != model.Steps() {
		t.Errorf("served %d of %d steps", model.Served(), model.Steps())
	}
}
