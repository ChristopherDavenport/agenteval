package agenteval_test

import (
	"context"
	"testing"

	"github.com/ChristopherDavenport/agenteval"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
)

// cut is a model whose every response the provider ends incomplete
// with reason, after the text, and a refusal part when refusal is set.
type cut struct {
	reason  openresponses.IncompleteReason
	refusal string
}

func (c cut) CreateStream(_ context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	em := openresponses.NewEmitter(sink, openresponses.NewResponse(req))
	w, err := em.Message("")
	if err != nil {
		return err
	}
	if err := w.Text(`{"score":1,"pass":true,"reason":"cut"}`); err != nil {
		return err
	}
	if c.refusal != "" {
		if err := w.Refusal(c.refusal); err != nil {
			return err
		}
	}
	return em.Incomplete(c.reason)
}

// TestStoredEndReadsAsTheLoopEnded: a run the provider cut, by its
// content filter or at the output limit, ends stopped with that cause
// (agentturn v0.0.40), and the recorder writes it aborted with the
// cause as its ref. Read back from its session the result says what the
// live one said, stopped and why, as it does for a guard's stop, which
// is written aborted too.
func TestStoredEndReadsAsTheLoopEnded(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name  string
		cfg   func() agentturn.Config
		cause agentturn.StopCause
	}{
		{name: "content filter", cause: agentturn.StopContentFilter, cfg: func() agentturn.Config {
			cfg := echoConfig()
			cfg.Model = cut{reason: openresponses.IncompleteReasonContentFilter, refusal: "I can't help with that."}
			return cfg
		}},
		{name: "output limit", cause: agentturn.StopMaxOutputTokens, cfg: func() agentturn.Config {
			cfg := echoConfig()
			cfg.Model = cut{reason: openresponses.IncompleteReasonMaxOutputTokens}
			return cfg
		}},
		{name: "guard", cause: agentturn.StopGuard, cfg: func() agentturn.Config {
			cfg := echoConfig()
			cfg.Tools = nil
			cfg.OutputGuard = func(context.Context, agentturn.OutputInfo) (*openresponses.Message, error) {
				return nil, agentturn.ErrGuard
			}
			return cfg
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newStableStore()
			r := &agenteval.Runner{Store: store, Config: func(agenteval.Task) agentturn.Config { return tt.cfg() }}
			report, err := r.Run(ctx, &agenteval.Suite{Name: "s", Tasks: []agenteval.Task{{ID: "one", Instruction: "hello"}}})
			if err != nil {
				t.Fatal(err)
			}
			live := report.Results[0]
			if live.Reason != agentturn.ReasonStopped || live.Cause != tt.cause || live.Err != nil {
				t.Fatalf("live reason = %s (%s), err = %v; want stopped (%s)", live.Reason, live.Cause, live.Err, tt.cause)
			}
			stored, err := agenteval.ResultAt(ctx, store, live.SessionID, live.Target)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Reason != live.Reason || stored.Cause != live.Cause {
				t.Errorf("stored reason = %s (%s), live %s (%s)", stored.Reason, stored.Cause, live.Reason, live.Cause)
			}
		})
	}
}
