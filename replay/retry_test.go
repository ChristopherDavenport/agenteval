package replay_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
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
		// Issue 36: any net.Error, as net/http wraps them.
		{"a read timeout, retried as transport", readTimeout, transportOnly, "i/o timeout"},
		{"a DNS failure, retried as transport", dnsFailure, transportOnly, "no such host"},
		{"a dial timeout, retried as transport", fmt.Errorf("dial tcp 10.0.0.1:443: %w", syscall.ETIMEDOUT), transportOnly, syscall.ETIMEDOUT.Error()},
		{"a client timeout, retried as transport", &url.Error{Op: "Post", URL: endpoint, Err: clientTimedOut{}}, transportOnly, "Client.Timeout exceeded"},
		// Issue 47: net/http's own timeouts, unexported net.Errors
		// inside a *url.Error.
		{"a response header timeout, retried as transport", headerTimeout, transportOnly, "timeout awaiting response headers"},
		{"a TLS handshake timeout, retried as transport", tlsTimeout, transportOnly, "TLS handshake timeout"},
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

// endpoint is the URL the transport failures below name.
const endpoint = "http://localhost:11434/v1/responses"

// readTimeout and dnsFailure are the errors net/http returns for a read
// that timed out and a host that does not resolve.
var (
	readTimeout = &url.Error{Op: "Post", URL: endpoint, Err: &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}}
	dnsFailure  = &url.Error{Op: "Post", URL: "http://ollama.internal:11434/v1/responses", Err: &net.OpError{Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "no such host", Name: "ollama.internal", IsNotFound: true}}}
)

// headerTimeout and tlsTimeout are the errors net/http returns when the
// Transport's ResponseHeaderTimeout or TLSHandshakeTimeout ends a
// request: an unexported net.Error that timed out, inside a *url.Error.
var (
	headerTimeout = &url.Error{Op: "Post", URL: endpoint, Err: httpTimeout("net/http: timeout awaiting response headers")}
	tlsTimeout    = &url.Error{Op: "Post", URL: "https://ollama.internal/v1/responses", Err: httpTimeout("net/http: TLS handshake timeout")}
)

// httpTimeout is a net.Error that timed out, with the text net/http
// gives its own.
type httpTimeout string

func (e httpTimeout) Error() string { return string(e) }
func (httpTimeout) Timeout() bool   { return true }
func (httpTimeout) Temporary() bool { return true }

// clientTimedOut is the error net/http's Client wraps when its Timeout
// ends a request.
type clientTimedOut struct{}

func (clientTimedOut) Error() string {
	return "context deadline exceeded (Client.Timeout exceeded while awaiting headers)"
}
func (clientTimedOut) Timeout() bool   { return true }
func (clientTimedOut) Temporary() bool { return true }

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

// Issue 36: a served failure names the transport failure its text
// names by type, so a Retryable reading the type decides as it did.
// Issue 47: one net/http wrapped as a *url.Error is served as one, with
// the URL, so a timeout net/http names only by text is still a net.Error
// that timed out, and DefaultRetryable retries the cause alone.
func TestAFailureNamesItsCause(t *testing.T) {
	// urlError reports whether err unwraps to a *url.Error naming the
	// endpoint, whose Timeout reports timeout.
	urlError := func(err error, at string, timeout bool) bool {
		var ue *url.Error
		var ne net.Error
		return errors.As(err, &ue) && ue.Op == "Post" && ue.URL == at && errors.As(err, &ne) && ne.Timeout() == timeout
	}
	tests := []struct {
		name  string
		err   error
		check func(error) bool
	}{
		{"a read timeout", readTimeout, func(err error) bool {
			return errors.Is(err, os.ErrDeadlineExceeded) && urlError(err, endpoint, true)
		}},
		{"a DNS failure", dnsFailure, func(err error) bool {
			var de *net.DNSError
			return errors.As(err, &de) && de.Name == "ollama.internal" && de.IsNotFound && urlError(err, "http://ollama.internal:11434/v1/responses", false)
		}},
		{"a client timeout", &url.Error{Op: "Post", URL: endpoint, Err: clientTimedOut{}}, func(err error) bool {
			return urlError(err, endpoint, true)
		}},
		{"a response header timeout", headerTimeout, func(err error) bool {
			var ue *url.Error
			return urlError(err, endpoint, true) && errors.As(err, &ue) && ue.Err.Error() == "net/http: timeout awaiting response headers"
		}},
		{"a TLS handshake timeout", tlsTimeout, func(err error) bool {
			return urlError(err, "https://ollama.internal/v1/responses", true)
		}},
		{"a refused dial", &url.Error{Op: "Post", URL: endpoint, Err: &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}}, func(err error) bool {
			return errors.Is(err, syscall.ECONNREFUSED) && urlError(err, endpoint, false)
		}},
		{"a context deadline", fmt.Errorf("stream: %w", context.DeadlineExceeded), func(err error) bool {
			return errors.Is(err, context.DeadlineExceeded)
		}},
		{"a refused connection", fmt.Errorf("dial tcp: %w", syscall.ECONNREFUSED), func(err error) bool {
			return errors.Is(err, syscall.ECONNREFUSED)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := fixtureConfig(&flaky{fail: map[int]bool{1: true}, err: tt.err})
			cfg.Retry = agentturn.Retry{MaxAttempts: 2, Backoff: func(int, error) time.Duration { return 0 }}
			orig, _, err := rerun(t, cfg, "first")
			if err != nil {
				t.Fatal(err)
			}
			model, err := replay.NewModel(orig, replay.Strict())
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = rerun(t, fixtureConfig(model), "first")
			var f *replay.Failure
			if !errors.As(err, &f) || f.Cause == nil || !tt.check(err) {
				t.Errorf("err = %v (%T cause), want a failure whose cause is the recorded one", err, causeOf(f))
			}
			// The cause alone is one agentturn's default policy retries,
			// as it retried the live error.
			if f != nil && f.Cause != nil && !agentturn.DefaultRetryable(f.Cause) {
				t.Errorf("DefaultRetryable(%v) = false", f.Cause)
			}
		})
	}
}

// Issue 47: a provider's message shaped like a *url.Error's text is not
// one, and names no cause.
func TestAQuotedMessageIsNotAURLError(t *testing.T) {
	said := &openresponses.Error{StatusCode: http.StatusServiceUnavailable, Type: openresponses.ErrorTypeServerError, Message: `upstream Said "x": timeout`}
	cfg := fixtureConfig(&flaky{fail: map[int]bool{1: true}, err: said})
	cfg.Retry = agentturn.Retry{MaxAttempts: 2, Backoff: func(int, error) time.Duration { return 0 }}
	orig, _, err := rerun(t, cfg, "first")
	if err != nil {
		t.Fatal(err)
	}
	model, err := replay.NewModel(orig, replay.Strict())
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = rerun(t, fixtureConfig(model), "first")
	var f *replay.Failure
	if !errors.As(err, &f) || f.Cause != nil || f.Err.HTTPStatus() != http.StatusServiceUnavailable {
		t.Errorf("err = %v (%T cause), want the 503 with no cause", err, causeOf(f))
	}
}

// causeOf is f's cause, for a message.
func causeOf(f *replay.Failure) error {
	if f == nil {
		return nil
	}
	return f.Cause
}

// Issue 36: the last attempt of a run whose retries ran out is a failed
// response, served as a Failure naming its step and entry.
func TestAFailedResponseIsAFailure(t *testing.T) {
	busy := &openresponses.Error{StatusCode: http.StatusInternalServerError, Type: openresponses.ErrorTypeServerError, Message: "upstream busy"}
	cfg := fixtureConfig(&flaky{fail: map[int]bool{1: true, 2: true}, err: busy})
	cfg.Retry = agentturn.Retry{MaxAttempts: 2, Backoff: func(int, error) time.Duration { return 0 }}
	orig, _, err := rerun(t, cfg, "first")
	if err == nil {
		t.Fatal("the recording did not fail")
	}
	var failedID string
	for _, e := range orig.Path(orig.Leaf()) {
		if r, ok := e.(*agentsession.ResponseEntry); ok && r.Status == openresponses.ResponseStatusFailed {
			failedID = r.ID
		}
	}
	if failedID == "" {
		t.Fatal("the recording holds no failed response")
	}
	model, err := replay.NewModel(orig, replay.Strict())
	if err != nil {
		t.Fatal(err)
	}
	cfg = fixtureConfig(model)
	cfg.Retry = agentturn.Retry{MaxAttempts: 2, Backoff: func(int, error) time.Duration { return 0 }}
	_, _, err = rerun(t, cfg, "first")
	var f *replay.Failure
	if !errors.As(err, &f) || f.EntryID != failedID || f.N != 2 {
		t.Fatalf("err = %v, want a failure at step 2 naming %s", err, failedID)
	}
	var oe *openresponses.Error
	if !errors.As(err, &oe) || oe.HTTPStatus() != http.StatusInternalServerError || f.Cause != nil {
		t.Errorf("err = %v, want the recorded 500 and no cause", err)
	}
}
