package agenteval_test

import (
	"context"
	"testing"

	"github.com/ChristopherDavenport/agenteval"
	"github.com/ChristopherDavenport/agentsession"
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
		// The hook stops a run whose last response requested nothing:
		// the recorder writes it done, with the cause as its ref.
		{name: "hook after an answer", cause: agentturn.StopHook, cfg: func() agentturn.Config {
			cfg := echoConfig()
			cfg.Tools = nil
			cfg.ShouldStopAfterTurn = func(context.Context, agentturn.TurnInfo) (bool, error) { return true, nil }
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

// TestStoredAbortReadsAsAborted: agentturn's recorder writes the
// host's abort, the loop's aborted, as interrupted, a reason the loop
// does not have (session.Recorder's endReason); read back it is
// aborted. The runner cannot abort a run without failing the batch, so
// the end is written as the recorder writes it.
func TestStoredAbortReadsAsAborted(t *testing.T) {
	ctx := context.Background()
	store := newStableStore()
	s, err := store.Create(ctx, agentsession.Header{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(ctx, s.ID(), agentsession.NewRunStart("run_1", agentsession.SourceInput, "")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Append(ctx, s.ID(), agentsession.NewRunEnd("run_1", agentsession.ReasonInterrupted, "context canceled", nil)); err != nil {
		t.Fatal(err)
	}
	stored, err := agenteval.ResultAt(ctx, store, s.ID(), "")
	if err != nil {
		t.Fatal(err)
	}
	if stored.Reason != agentturn.ReasonAborted || stored.Cause != "" {
		t.Errorf("stored reason = %s (%s), want aborted", stored.Reason, stored.Cause)
	}
}
