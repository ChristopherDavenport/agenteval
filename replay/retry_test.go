package replay_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ChristopherDavenport/agenteval/replay"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/session"
	"github.com/ChristopherDavenport/openresponses"
	"github.com/ChristopherDavenport/openresponses/echo"
)

// flaky fails the calls numbered in fail, from 1, with err, and passes
// the rest to echo.
type flaky struct {
	mu   sync.Mutex
	n    int
	fail map[int]bool
	err  error
}

func (f *flaky) CreateStream(ctx context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	f.mu.Lock()
	f.n++
	fail := f.fail[f.n]
	f.mu.Unlock()
	if fail {
		return f.err
	}
	return (&echo.Adapter{}).CreateStream(ctx, req, sink)
}

// A retry the record holds is served as a failure, so the loop retries
// and a Retry.Revise that fell back to another model sends the
// request the record hashed: a strict replay serves every step.
func TestStrictReplaysARevisedRetry(t *testing.T) {
	limited := &openresponses.Error{StatusCode: http.StatusTooManyRequests, Type: openresponses.ErrorTypeTooManyRequests, Message: "slow down"}
	onlyLimits := func(err error) bool {
		var oe *openresponses.Error
		return errors.As(err, &oe) && oe.HTTPStatus() == http.StatusTooManyRequests
	}
	tests := []struct {
		name      string
		err       error
		retryable func(error) bool
		want      string
	}{
		{"a rate limit", limited, nil, limited.Error()},
		{"a rate limit, retried on its status", limited, onlyLimits, limited.Error()},
		{"a truncated stream", io.ErrUnexpectedEOF, nil, io.ErrUnexpectedEOF.Error()},
		// Issue 34: a product whose gateway retries status errors
		// retries only transport failures in process.
		{"a truncated stream, retried as transport", openresponses.ErrTruncatedStream, transportOnly, openresponses.ErrTruncatedStream.Error()},
		{"a cut connection, retried as transport", io.ErrUnexpectedEOF, transportOnly, io.ErrUnexpectedEOF.Error()},
		{"a reset, retried as transport", fmt.Errorf("read tcp 10.0.0.1:443: %w", syscall.ECONNRESET), transportOnly, syscall.ECONNRESET.Error()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			retry := agentturn.Retry{
				MaxAttempts: 2,
				Backoff:     func(int, error) time.Duration { return 0 },
				Retryable:   tt.retryable,
				Revise: func(_ int, req *openresponses.Request, _ error) *openresponses.Request {
					req.Model = "echo/echo-fallback"
					return nil
				},
			}
			cfg := fixtureConfig(&flaky{fail: map[int]bool{1: true}, err: tt.err})
			cfg.Retry = retry
			orig, _, err := rerun(t, cfg, "first", "second")
			if err != nil {
				t.Fatal(err)
			}
			var retries int
			for _, e := range orig.Path(orig.Leaf()) {
				if c, ok := e.(*agentsession.CustomEntry); ok && c.NS == session.ModelRetryNS {
					retries++
				}
			}
			if retries != 1 {
				t.Fatalf("the recording holds %d retries, want 1", retries)
			}

			var seen []replay.Served
			model, err := replay.NewModel(orig, replay.Strict(), replay.WithObserver(func(sv replay.Served) { seen = append(seen, sv) }))
			if err != nil {
				t.Fatal(err)
			}
			// The live configuration keeps its policy and its own
			// backoff; the failure's Retry-After makes DefaultBackoff
			// wait for nothing.
			cfg = fixtureConfig(model)
			retry.Backoff = nil
			cfg.Retry = retry
			start := time.Now()
			s, end, err := rerun(t, cfg, "first", "second")
			if err != nil {
				t.Fatalf("strict replay: %v", err)
			}
			if d := time.Since(start); d > 400*time.Millisecond {
				t.Errorf("the replay waited %v before retrying", d)
			}
			if end.Reason != agentturn.ReasonDone {
				t.Errorf("reason = %s", end.Reason)
			}
			if model.Served() != model.Steps() || model.Steps() != 3 {
				t.Errorf("served %d of %d steps, want 3", model.Served(), model.Steps())
			}
			if len(seen) == 0 || seen[0].Kind != replay.KindFailure {
				t.Fatalf("served %+v, want a failure first", seen)
			}
			if want, got := hashes(orig), hashes(s); strings.Join(want, ",") != strings.Join(got, ",") {
				t.Errorf("replayed hashes\n got %v\nwant %v", got, want)
			}
			// The replayed record carries the failure's text.
			var rec string
			for _, e := range s.Path(s.Leaf()) {
				if c, ok := e.(*agentsession.CustomEntry); ok && c.NS == session.ModelRetryNS {
					rec = string(c.Data)
				}
			}
			if !strings.Contains(rec, tt.want) || !strings.Contains(rec, `"revised":true`) {
				t.Errorf("replayed retry = %s, want the text %q, revised", rec, tt.want)
			}
			// A replayed run replays as the recording did.
			again, err := replay.NewModel(s, replay.Strict())
			if err != nil {
				t.Fatal(err)
			}
			cfg = fixtureConfig(again)
			cfg.Retry = retry
			if _, _, err := rerun(t, cfg, "first", "second"); err != nil {
				t.Fatalf("strict replay of the replay: %v", err)
			}
		})
	}
}

// transportOnly retries the transport failures alone, leaving status
// errors to a gateway.
func transportOnly(err error) bool {
	var ne net.Error
	return errors.Is(err, openresponses.ErrTruncatedStream) || errors.Is(err, io.ErrUnexpectedEOF) || errors.As(err, &ne)
}

// Issue 34: a replay under a configuration that does not retry ends on
// the recorded failure, and the error says which step and entry it
// was, and still unwraps to what the recorded text names.
func TestAReplayThatStopsAtARecordedFailure(t *testing.T) {
	cfg := fixtureConfig(&flaky{fail: map[int]bool{1: true}, err: io.ErrUnexpectedEOF})
	cfg.Retry = agentturn.Retry{MaxAttempts: 2, Backoff: func(int, error) time.Duration { return 0 }}
	orig, _, err := rerun(t, cfg, "first")
	if err != nil {
		t.Fatal(err)
	}
	var retryID string
	for _, e := range orig.Path(orig.Leaf()) {
		if c, ok := e.(*agentsession.CustomEntry); ok && c.NS == session.ModelRetryNS {
			retryID = c.ID
		}
	}
	for _, strict := range []bool{true, false} {
		var opts []replay.Option
		if strict {
			opts = append(opts, replay.Strict())
		}
		model, err := replay.NewModel(orig, opts...)
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = rerun(t, fixtureConfig(model), "first")
		var f *replay.Failure
		if !errors.As(err, &f) {
			t.Fatalf("strict %v: err = %v, want a replay.Failure", strict, err)
		}
		if f.N != 1 || f.EntryID != retryID {
			t.Errorf("strict %v: failure at step %d entry %s, want step 1 entry %s", strict, f.N, f.EntryID, retryID)
		}
		if !errors.Is(err, io.ErrUnexpectedEOF) || !strings.Contains(err.Error(), retryID) {
			t.Errorf("strict %v: err = %v, want it to unwrap to io.ErrUnexpectedEOF and name %s", strict, err, retryID)
		}
		var oe *openresponses.Error
		if !errors.As(err, &oe) || oe.HTTPStatus() != http.StatusServiceUnavailable {
			t.Errorf("strict %v: err = %v, want the 503 it is served as", strict, err)
		}
	}
}

// A compaction where the recording failed a call is a divergence.
func TestCompactWhereTheRecordingFailed(t *testing.T) {
	cfg := fixtureConfig(&flaky{fail: map[int]bool{1: true}, err: io.ErrUnexpectedEOF})
	cfg.Retry = agentturn.Retry{MaxAttempts: 2, Backoff: func(int, error) time.Duration { return 0 }}
	orig, _, err := rerun(t, cfg, "first")
	if err != nil {
		t.Fatal(err)
	}
	model, err := replay.NewModel(orig)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := model.Compact(context.Background(), openresponses.CompactRequest{}); !errors.Is(err, replay.ErrDiverged) {
		t.Errorf("Compact = %v, want ErrDiverged", err)
	}
}
