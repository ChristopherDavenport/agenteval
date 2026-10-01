package replay_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agenteval/replay"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/compact"
	"github.com/ChristopherDavenport/agentturn/session"
	"github.com/ChristopherDavenport/openresponses"
	"github.com/ChristopherDavenport/openresponses/echo"
)

// summarizer answers a fold's summary call with summary and passes
// every other call to echo.
type summarizer struct {
	summary func(req openresponses.Request, sink openresponses.EventSink) error
}

func (s summarizer) CreateStream(ctx context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	if len(req.Tools) == 0 && req.Instructions == "" {
		return s.summary(req, sink)
	}
	return (&echo.Adapter{}).CreateStream(ctx, req, sink)
}

// echoSummary answers with echo, whose summary repeats what it folds
// and so is always too large.
func echoSummary(req openresponses.Request, sink openresponses.EventSink) error {
	return (&echo.Adapter{}).CreateStream(context.Background(), req, sink)
}

// emptyResponse ends a response with no output, incomplete for reason
// or completed when reason is empty.
func emptyResponse(req openresponses.Request, sink openresponses.EventSink, reason openresponses.IncompleteReason) error {
	resp := openresponses.NewResponse(req)
	resp.Usage = &openresponses.Usage{InputTokens: 7, OutputTokens: 3, TotalTokens: 10}
	em := openresponses.NewEmitter(sink, resp)
	if err := em.Start(); err != nil {
		return err
	}
	if reason != "" {
		return em.Incomplete(reason)
	}
	return em.Complete()
}

// failedFolds returns the data of the compaction_failed entries on the
// path to the session's leaf, with the response ID left out: a replay
// serves the recorded one only where the record kept it.
func failedFolds(t *testing.T, s *agentsession.Session) []session.FailedFold {
	t.Helper()
	var out []session.FailedFold
	for _, e := range s.Path(s.Leaf()) {
		c, ok := e.(*agentsession.CustomEntry)
		if !ok || c.NS != session.FailedFoldNS {
			continue
		}
		var f session.FailedFold
		if err := json.Unmarshal(c.Data, &f); err != nil {
			t.Fatal(err)
		}
		out = append(out, f)
	}
	return out
}

// A fold that failed is served so it fails again as it did, whichever
// way it failed, and a strict replay checks each of its calls.
func TestStrictReplaysAFailedFold(t *testing.T) {
	tests := []struct {
		name string
		// summary answers the recording's summary calls.
		summary func(openresponses.Request, openresponses.EventSink) error
		// fails is whether the failed fold fails the turn.
		fails bool
	}{
		{"too large", echoSummary, false},
		{"incomplete", func(req openresponses.Request, sink openresponses.EventSink) error {
			return emptyResponse(req, sink, openresponses.IncompleteReasonMaxOutputTokens)
		}, false},
		{"no text", func(req openresponses.Request, sink openresponses.EventSink) error {
			return emptyResponse(req, sink, "")
		}, true},
		{"the call failed", func(openresponses.Request, openresponses.EventSink) error {
			return &openresponses.Error{StatusCode: http.StatusBadGateway, Type: openresponses.ErrorTypeServerError, Message: "bad gateway"}
		}, true},
	}
	fold := func(model openresponses.Streamer) func(*agentturn.Config, *session.Recorder) {
		return func(cfg *agentturn.Config, rec *session.Recorder) {
			cfg.Transform = compact.NewLocal(model, compact.WithBudget(1), compact.WithKeepLast(2), compact.WithOnFold(rec.Fold)).Transform
		}
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			live := summarizer{summary: tt.summary}
			orig, _, origErr := rerunWith(t, fixtureConfig(live), fold(live), "one", "two")
			if (origErr != nil) != tt.fails {
				t.Fatalf("recording: err = %v, want failure %v", origErr, tt.fails)
			}
			want := failedFolds(t, orig)
			if len(want) == 0 {
				t.Fatal("the recording holds no failed fold")
			}

			var seen []replay.Served
			model, err := replay.NewModel(orig, replay.Strict(), replay.WithObserver(func(sv replay.Served) { seen = append(seen, sv) }))
			if err != nil {
				t.Fatal(err)
			}
			s, _, err := rerunWith(t, fixtureConfig(model), fold(model), "one", "two")
			if (err != nil) != tt.fails || (err != nil && err.Error() != origErr.Error()) {
				t.Fatalf("strict replay: err = %v, want %v", err, origErr)
			}
			if model.Served() != model.Steps() {
				t.Errorf("served %d of %d steps", model.Served(), model.Steps())
			}
			failed := 0
			for _, sv := range seen {
				if sv.Kind != replay.KindFailedFold {
					continue
				}
				failed++
				if !sv.Match {
					t.Errorf("failed fold at step %d: recorded %s, got %s", sv.N, sv.Recorded, sv.Got)
				}
			}
			if attempts := want[0].Attempts; failed < attempts {
				t.Errorf("served %d failed fold calls, the first fold alone made %d", failed, attempts)
			}
			if w, g := hashes(orig), hashes(s); strings.Join(w, ",") != strings.Join(g, ",") {
				t.Errorf("replayed hashes\n got %v\nwant %v", g, w)
			}
			got := failedFolds(t, s)
			if len(got) != len(want) {
				t.Fatalf("replayed %d failed folds, recorded %d", len(got), len(want))
			}
			for i := range want {
				w, g := want[i], got[i]
				if foldError(g.Error) != foldError(w.Error) || g.Attempts != w.Attempts || g.RequestHash != w.RequestHash || !sameUsage(g.Usage, w.Usage) {
					t.Errorf("failed fold %d\n got %+v\nwant %+v", i, g, w)
				}
			}
		})
	}
}

// foldError is a failed fold's error less the estimates a too-large
// one carries: the replay serves a summary of its own making, whose
// size is not the recorded one's, only as sure to be too large.
func foldError(text string) string {
	if strings.HasPrefix(text, compact.ErrSummaryTooLarge.Error()) {
		return compact.ErrSummaryTooLarge.Error()
	}
	return text
}

func sameUsage(a, b *openresponses.Usage) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// A fold whose request differs from the one the failed fold recorded
// is refused under a strict replay.
func TestStrictChecksAFailedFold(t *testing.T) {
	live := summarizer{summary: echoSummary}
	setup := func(model openresponses.Streamer, opts ...compact.Option) func(*agentturn.Config, *session.Recorder) {
		return func(cfg *agentturn.Config, rec *session.Recorder) {
			opts = append(opts, compact.WithBudget(1), compact.WithKeepLast(2), compact.WithOnFold(rec.Fold))
			cfg.Transform = compact.NewLocal(model, opts...).Transform
		}
	}
	orig, _, err := rerunWith(t, fixtureConfig(live), setup(live), "one", "two")
	if err != nil {
		t.Fatal(err)
	}
	model, err := replay.NewModel(orig, replay.Strict())
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = rerunWith(t, fixtureConfig(model), setup(model, compact.WithSummaryPrompt("Write a haiku.")), "one", "two")
	if !errors.Is(err, replay.ErrDiverged) || !strings.Contains(err.Error(), "failed fold") {
		t.Fatalf("err = %v, want ErrDiverged naming the failed fold", err)
	}
}
