// Package replay serves a recorded session as a model and as tools, so
// hooks, transforms and fronts are tested against real traffic with
// nothing behind them.
//
// [Model] serves the session's recorded model calls in path order:
// each response entry on the path is one call, served with the output
// items recorded before it, item entries naming its response ID and,
// from agentturn v0.0.15, custom entries marked with it, which hold an
// output item the filter kept from the model; each compaction entry
// is the fold that preceded the call after it, and each
// agentturn:compaction_failed entry is a fold that failed there, one
// call per attempt it made. In strict mode a
// request whose hash differs from the one recorded on the entry about
// to be served is refused with [ErrDiverged], a fold's own request
// included, so a summary prompt, a budget or a filter that changed is
// not signed off as neutral; a call the record carries no hash for is
// refused with [ErrUnverifiable] rather than served unchecked, unless
// [AllowUnhashed] says to serve it. The exception is a fold through
// the compaction endpoint, which sends no request the format hashes
// and so is served unchecked in every mode: see [Model.Compact]. In
// lenient mode the calls are served by position and the hashes are
// reported through the observer. [Tools] wraps a tool list so that a
// call matching a recorded function call returns its recorded output
// instead of running.
//
// A model and its tools are not the whole of what a call was made
// under. [Model.BeforeModelCall] serves back the instructions and the
// tool list in force at each recorded call, so a product whose layers
// rebuild them every turn from a store, a skill set or a memory block
// is replayed against the run rather than against what those layers
// would build today; [Model.Settings] holds the rest of what each
// call was made under.
//
//	s, _ := store.Open(ctx, id)
//	model, _ := replay.NewModel(s, replay.Strict())
//	cfg.Model = model
//	cfg.BeforeModelCall = model.BeforeModelCall
//	cfg.Tools = replay.Tools(s, cfg.Tools, replay.Strict())
package replay

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentturn/compact"
	"github.com/ChristopherDavenport/agentturn/session"
	"github.com/ChristopherDavenport/openresponses"
)

// ErrDiverged is returned by a strict model when the request it is
// handed does not hash to the value recorded on the entry it would
// serve, and by strict tools for a call with no recorded output.
var ErrDiverged = errors.New("replay: diverged from the recording")

// ErrUnverifiable is returned when the record carries no hash for a
// call a strict replay would serve, so nothing can be checked against
// what the recording sent. It is not [ErrDiverged]: nothing was
// measured to differ, and the record simply does not say. [NewModel]
// returns it for a path whose responses are not all hashed, and a
// strict fold whose compaction entry recorded no fold hash returns it
// at the call; [AllowUnhashed] serves both unchecked instead. A path
// whose workspace was substituted is refused with it too, as
// [ErrSubstituted].
var ErrUnverifiable = errors.New("replay: the record cannot say what was sent")

// ErrSubstituted is returned by a strict [NewModel] for a path on which
// an env entry after a response names another workspace than the one
// in force before it, which before the first env entry is none: RFC
// 0001 calls that a substitution, and a reader that holds the environment
// fixed treats the path from it on as not verifiable against what came
// before it. The recorded outputs after it came from another file
// system than those before it, so a pass on one half says nothing
// about the other. It wraps [ErrUnverifiable]: nothing was measured to
// differ, and the record cannot be held to one environment.
// [AllowSubstitution] serves the path anyway.
var ErrSubstituted = fmt.Errorf("%w: the path changes workspace", ErrUnverifiable)

// ErrCallIDRepeated is returned by a strict [NewModel] for a path on
// which two function calls share a call ID, as a session recorded by
// agentturn v0.0.11 or earlier holds when its provider numbered calls
// per response. agentturn from v0.0.12 gives the second call an ID of
// its own, so the request after it cannot hash to what the recording
// sent, and a strict replay would diverge there naming two hashes and
// nothing about call IDs. It wraps [ErrUnverifiable], and no option
// serves such a path strictly; a lenient replay serves it, and
// [Tools] serves the repeated call's recorded output.
var ErrCallIDRepeated = fmt.Errorf("%w: the path repeats a call ID", ErrUnverifiable)

// ErrExhausted is returned when the path holds no further recorded
// call of the kind requested.
var ErrExhausted = errors.New("replay: no further recorded call on the path")

// Kind says what a [Served] value describes.
type Kind string

// Kinds of served value.
const (
	// KindResponse is a model call served from a response entry.
	KindResponse Kind = "response"
	// KindFold is a fold served from a compaction entry.
	KindFold Kind = "fold"
	// KindFailure is a failed attempt served from an
	// agentturn:model_retry entry, as the error the loop retries.
	KindFailure Kind = "failure"
	// KindFailedFold is one summary call of a fold that failed, served
	// from an agentturn:compaction_failed entry so the fold fails
	// again as it did.
	KindFailedFold Kind = "failed_fold"
	// KindCall is a tool call served from a recorded output.
	KindCall Kind = "call"
)

// Served is what the model and the tools report through the observer
// for each thing they serve.
type Served struct {
	Kind Kind
	// N is the 1-based position among the served calls of the model,
	// or of the tools.
	N int
	// EntryID is the entry served: the response, compaction or
	// model_retry entry for the model, the output's item entry for a
	// tool call.
	EntryID string
	// Recorded and Got are the hash recorded for the call and the hash
	// of the request received: the response entry's own hash, or, for
	// a fold, the one on its compaction entry's fold member. An entry
	// written before agentturn v0.0.6 carries no fold member at all,
	// and a response entry carries no hash when the path does not
	// rebuild that request's input; both leave Recorded empty, and a
	// strict model reports them only under [AllowUnhashed], having
	// otherwise refused the call.
	//
	// A fold served through [Model.Compact] is the exception, and the
	// one case where an empty Recorded on a strict replay does not mean
	// the caller opted out: the compaction endpoint sends no request
	// the format hashes, so that fold is served unchecked in every
	// mode, and Got is always empty there whatever Recorded holds.
	//
	// A failed attempt has no Recorded: the recorder hashes only the
	// request of the attempt that answered, and that one is checked
	// when it is served. Got is the hash of what the failed attempt
	// sent.
	//
	// Both are empty for a tool call, which is matched on its call ID
	// or its arguments rather than on a hash.
	//
	// Match reports whether the two agree, and is false when there was
	// nothing to compare. A model call — [KindResponse], [KindFold] or
	// [KindFailedFold] —
	// was checked exactly when Recorded and Got are both set, so Match
	// on its own is not a statement that it was. A tool call is the
	// other way round: it is reported only when a recorded output was
	// found, so its Match is always true and its hashes are never
	// consulted.
	Recorded, Got string
	Match         bool
	// CallID and Name describe a served tool call, and ByID reports
	// whether it matched on call ID rather than on name and arguments.
	CallID, Name string
	ByID         bool
}

type options struct {
	strict        bool
	allowUnhashed bool
	allowSubst    bool
	leaf          string
	observer      func(Served)
	foldText      func(openresponses.Item) string
	details       map[string]func(json.RawMessage) (any, error)
}

// Option configures a [Model] or [Tools].
type Option func(*options)

// Strict makes a model refuse a request whose hash differs from the
// recorded one, and tools refuse a call with no recorded output. A
// fold is checked against the hash its compaction entry recorded for
// it as a response is against its own.
//
// An entry that recorded no hash is refused rather than served
// unchecked, with [ErrUnverifiable]: [NewModel] refuses the whole
// session when a response on the path carries no request hash, before
// a call is served, and a fold whose compaction entry recorded no fold
// hash is refused at the call, because whether that matters depends on
// the compactor the replay is run with. [AllowUnhashed] serves them
// instead, which is how a recording made before agentturn v0.0.6
// replays.
//
// One call is served unchecked whatever is set: a fold through the
// compaction endpoint, which sends no request the format hashes, so
// there is nothing for strict mode to check and nothing for
// [AllowUnhashed] to allow. See [Model.Compact].
//
// The default is lenient: the model serves by position and reports the
// hashes through the observer, and an unmatched call runs the real
// tool.
func Strict() Option { return func(o *options) { o.strict = true } }

// AllowUnhashed lets a strict model serve a call whose entry recorded
// no hash: by position, and unchecked. It turns the guarantee off for
// those calls rather than relaxing it. What the caller gives up is the
// whole of what [Strict] is for — that every served call was checked
// against the request the recording made — for every call the record
// is silent about, and the replay's own result — that it finished
// without [ErrDiverged] — no longer distinguishes a call that was
// checked and matched from one that was never checked. Only an
// observer subscribing to [Served] can tell them apart afterwards: a
// model call was checked exactly when its Recorded and Got are both
// set.
//
// It exists for recordings the format cannot describe: one made before
// agentturn v0.0.6, which wrote no fold member, and one whose requests
// a transform or a hook edited. Reach for it to replay an old session
// at all, not to quiet a failure on a current one.
func AllowUnhashed() Option { return func(o *options) { o.allowUnhashed = true } }

// AllowSubstitution lets a strict model serve a path whose workspace
// was substituted part way, which it otherwise refuses with
// [ErrSubstituted]. The calls are still checked against their hashes;
// what the caller accepts is that the tool outputs before and after the
// substitution came from different file systems, as when a session
// recorded in a container was resumed on a laptop and the replay is
// meant to cover both halves.
func AllowSubstitution() Option { return func(o *options) { o.allowSubst = true } }

// WithLeaf names the path to serve. The default is the session's
// current leaf, which after judging is an outcome entry rather than a
// model output; a branched session has several leaves and a replay
// names the one it wants.
func WithLeaf(id string) Option { return func(o *options) { o.leaf = id } }

// WithObserver sets a function called for everything served.
func WithObserver(fn func(Served)) Option { return func(o *options) { o.observer = fn } }

// WithFoldText says how the summary item of a compaction entry becomes
// the text the model answered a local fold with. The default undoes
// compact.SummaryMessage; a configuration that folds with its own
// WithSummaryItem passes the inverse here.
func WithFoldText(fn func(summary openresponses.Item) string) Option {
	return func(o *options) { o.foldText = fn }
}

// summaryPrefix is what compact.SummaryMessage puts before the text.
const summaryPrefix = "Summary of the conversation so far:\n\n"

// DefaultFoldText is the default [WithFoldText]: the text of a message
// summary with compact.SummaryMessage's prefix removed, or the text of
// any other item.
func DefaultFoldText(summary openresponses.Item) string {
	if m, ok := summary.(*openresponses.Message); ok {
		return strings.TrimPrefix(m.Text(), summaryPrefix)
	}
	if c, ok := summary.(*openresponses.Compaction); ok {
		return c.EncryptedContent
	}
	return ""
}

func apply(opts []Option) options {
	o := options{foldText: DefaultFoldText}
	for _, opt := range opts {
		opt(&o)
	}
	return o
}

// step is one recorded model call on the path: a response with the
// items it produced, a fold with its summary, or an attempt that
// failed and was retried.
type step struct {
	resp    *agentsession.ResponseEntry
	output  openresponses.Items
	comp    *agentsession.CompactionEntry
	summary openresponses.Item
	// retry is the model_retry entry of a failed attempt, and failure
	// the error text it recorded, which it is served as.
	retry   *agentsession.CustomEntry
	failure string
	// failed is the compaction_failed entry of a fold that failed, and
	// fold its data. A fold that asked twice is two steps, attempt 1
	// and 2 of calls, which foldCalls reads from the record.
	failed  *agentsession.CustomEntry
	fold    session.FailedFold
	attempt int
	calls   int
	// settings are the settings in force at this step: what the config
	// entries on the path up to it say the request was made under.
	settings agentsession.Settings
	// foldHash is the hash of the request the recorded fold sent, from
	// the compaction entry's fold member. It is empty for a fold
	// through the compaction endpoint, which sends no request the
	// format hashes, and for an entry written before agentturn
	// v0.0.6, which recorded no fold member at all.
	foldHash string
}

// Model is an openresponses.Streamer, and a compact.Compactor, served
// from a recorded session. It is safe for concurrent use, though a
// loop calls it from one goroutine at a time.
type Model struct {
	opts  options
	steps []step

	mu   sync.Mutex
	next int
	// folded is the hash of the request the last fold step served
	// answered, and foldedAt that step.
	folded   string
	foldedAt int
}

// NewModel builds a model over the path from the root to the leaf
// named by [WithLeaf], or to the session's current leaf. A strict
// model over a path a strict replay could not check is refused here
// with [ErrUnverifiable], rather than at the call it could not check:
// see [Unverifiable], and [AllowUnhashed] and [AllowSubstitution] to
// serve it anyway.
func NewModel(s *agentsession.Session, opts ...Option) (*Model, error) {
	o := apply(opts)
	path, err := pathTo(s, o.leaf)
	if err != nil {
		return nil, err
	}
	if o.strict {
		if err := unverifiable(path, o); err != nil {
			return nil, err
		}
	}
	m := &Model{opts: o}
	// The settings in force at each step are the config entries on the
	// path applied in order, which is what a product whose layers
	// re-read state each turn must send to replay strictly: the
	// recording's instructions, not the ones those layers would build
	// again today.
	var settings agentsession.Settings
	for i, e := range path {
		switch v := e.(type) {
		case *agentsession.ConfigEntry:
			settings = settings.Apply(v)
		case *agentsession.ResponseEntry:
			st := step{resp: v, settings: settings}
			// The response's own output is the item entries before it
			// that name its response ID, and the custom entries marked
			// with it: from agentturn v0.0.15 an output item the filter
			// keeps from the model, a text-call parser's raw item say,
			// is written as a custom entry in the namespace of its type
			// marked with its response ID, outside the context and the
			// requests, and a replay that skipped it served the
			// response short of it with nothing to say so (#50). Other
			// entries are skipped and the walk stops at the first item
			// entry, or marked entry, belonging to something else. It
			// is the rule Session.RequestContext uses to drop them, and
			// the two must agree or a replay serves an input item as
			// output. Skipping rather than stopping is what lets an
			// observer write a custom entry between two output items of
			// one response, which is where every layer above the loop
			// puts its verdict, without losing the item after it.
			if v.ResponseID != "" {
			walk:
				for j := i - 1; j >= 0; j-- {
					switch e := path[j].(type) {
					case *agentsession.ItemEntry:
						if e.ResponseID != v.ResponseID {
							break walk
						}
						st.output = append(openresponses.Items{e.Item}, st.output...)
					case *agentsession.CustomEntry:
						item, responseID, ok := markedItem(e)
						if !ok {
							continue
						}
						if responseID != v.ResponseID {
							break walk
						}
						st.output = append(openresponses.Items{item}, st.output...)
					}
				}
			}
			// Served items are cloned: what is served reaches the
			// replayed run's transcript and its recorder, and the
			// entries of the session being replayed must not be
			// reachable from there.
			st.output = st.output.Clone()
			m.steps = append(m.steps, st)
		case *agentsession.CompactionEntry:
			m.steps = append(m.steps, step{comp: v, summary: openresponses.Items{v.Summary}.Clone()[0], foldHash: foldHash(v), settings: settings})
		case *agentsession.CustomEntry:
			if v.NS == session.FailedFoldNS {
				var f session.FailedFold
				if err := json.Unmarshal(v.Data, &f); err != nil {
					return nil, fmt.Errorf("replay: failed fold %s: %w", v.ID, err)
				}
				// Each summary call the fold made is a step.
				calls := foldCalls(f)
				for a := range calls {
					m.steps = append(m.steps, step{failed: v, fold: f, attempt: a + 1, calls: calls, foldHash: f.RequestHash, settings: settings})
				}
				continue
			}
			// A failed attempt is served as a failure so the loop
			// retries it, and a Retry.Revise that changed the next
			// request, to a fallback model say, changes it again:
			// the response after it is hashed over the revised
			// request, and a replay that never failed would send the
			// unrevised one and diverge there.
			if v.NS != session.ModelRetryNS {
				continue
			}
			var rec session.ModelRetry
			if err := json.Unmarshal(v.Data, &rec); err != nil {
				return nil, fmt.Errorf("replay: model retry %s: %w", v.ID, err)
			}
			m.steps = append(m.steps, step{retry: v, failure: rec.Error, settings: settings})
		}
	}
	return m, nil
}

// recordedError matches the text of an openresponses.Error:
// "openresponses: <type> (<status>): <message>".
var recordedError = regexp.MustCompile(`^openresponses: ([a-z_]+) \((\d{3})\)(?:: (.*))?$`)

// servedFailure matches the prefix a [Failure] puts on the text, so a
// replay of a replayed run rebuilds the error the first one served.
var servedFailure = regexp.MustCompile(`^replay: recorded failure at step \d+ \([^)]*\): `)

// Failure is the error a replay model serves for a failed attempt the
// record holds: an agentturn:model_retry entry, a fold's summary call
// that failed, or a response entry that recorded a failed response,
// the last attempt of a run whose retries ran out. It names the step
// and the entry it was served from, so a run that ends on it, under a
// configuration that gives up where the recording retried, says it
// ended on a recorded failure rather than reading as a provider
// outage. It unwraps to Err, the openresponses error its recorded text
// spells or a 503 carrying the text, so a Retry.Retryable that reads
// the status decides as it did, and to Cause, the transport failure
// the text names when it names one, so a Retryable that retries only
// transport failures retries it too. The texts recognised, as the
// whole text or its last colon-separated part, are those of
// openresponses.ErrTruncatedStream, io.ErrUnexpectedEOF, io.EOF,
// os.ErrDeadlineExceeded ("i/o timeout"), context.DeadlineExceeded,
// and syscall.ECONNRESET, ECONNREFUSED, EPIPE, ETIMEDOUT, ENETUNREACH
// and EHOSTUNREACH, each served as itself; "no such host", served as
// a *net.DNSError; and a net/http client timeout, served as a
// net.Error whose Timeout is true. Each is a net.Error or one of the
// errors agentturn's DefaultRetryable names. A Retryable that declines
// every openresponses error before asking about the transport
// declines it: the record keeps the text alone, and the 503 is what
// gives it Retry-After. A failed response has no Cause and is served
// as the error it recorded.
type Failure struct {
	// N is the step, numbered as [Served.N] is, and EntryID the entry
	// the failure was served from.
	N       int
	EntryID string
	Err     *openresponses.Error
	Cause   error
}

func (f *Failure) Error() string {
	return fmt.Sprintf("replay: recorded failure at step %d (%s): %s", f.N, f.EntryID, f.Err.Error())
}

// Unwrap returns Err, and Cause when there is one.
func (f *Failure) Unwrap() []error {
	if f.Cause == nil {
		return []error{f.Err}
	}
	return []error{f.Err, f.Cause}
}

// transportFailures are the errors a recorded text may end with that a
// Failure unwraps to, longest text first so "unexpected EOF" is not
// read as "EOF".
var transportFailures = []error{
	openresponses.ErrTruncatedStream,
	context.DeadlineExceeded,
	syscall.ECONNREFUSED,
	syscall.EHOSTUNREACH,
	syscall.ENETUNREACH,
	syscall.ETIMEDOUT,
	syscall.ECONNRESET,
	io.ErrUnexpectedEOF,
	os.ErrDeadlineExceeded,
	syscall.EPIPE,
	io.EOF,
}

// lookupFailure matches the text of a *net.DNSError for a host that
// does not resolve: "lookup <name>[ on <server>]: no such host".
var lookupFailure = regexp.MustCompile(`lookup (\S+?)(?: on (\S+))?: no such host$`)

// clientTimeout is the text net/http's Client puts on a request its
// Timeout ended, after the error it wraps.
const clientTimeout = "(Client.Timeout exceeded while awaiting headers)"

// causeOf returns the transport failure text names, as the whole text
// or as the last of its colon-separated parts, or nil.
func causeOf(text string) error {
	for _, err := range transportFailures {
		if t := err.Error(); text == t || strings.HasSuffix(text, ": "+t) {
			return err
		}
	}
	if m := lookupFailure.FindStringSubmatch(text); m != nil {
		return &net.DNSError{Err: "no such host", Name: m[1], Server: m[2], IsNotFound: true}
	}
	if strings.HasSuffix(text, clientTimeout) {
		return timeoutError(text)
	}
	return nil
}

// timeoutError is a net.Error that timed out, for a recorded timeout
// whose own type the text does not name.
type timeoutError string

func (e timeoutError) Error() string { return string(e) }

// Timeout reports true.
func (timeoutError) Timeout() bool { return true }

// Temporary reports true, as net/http's own timeout does.
func (timeoutError) Temporary() bool { return true }

// serveFailure returns the failure recorded as text at step n of entry.
func serveFailure(n int, entryID, text string) *Failure {
	text = servedFailure.ReplaceAllString(text, "")
	return &Failure{N: n, EntryID: entryID, Err: failureOf(text), Cause: causeOf(text)}
}

// failureOf is the error a failed attempt is served as: the
// openresponses error its recorded text spells, so a Retry.Retryable
// that reads the status decides as it did, or a 503 carrying the text
// when it spells none, as a truncated stream or a transport failure
// does. It carries Retry-After: 0, so agentturn's DefaultBackoff waits
// for nothing; a product's own Backoff still waits what it says.
func failureOf(text string) *openresponses.Error {
	e := &openresponses.Error{StatusCode: http.StatusServiceUnavailable, Type: openresponses.ErrorTypeServerError, Message: text}
	if m := recordedError.FindStringSubmatch(text); m != nil {
		status, _ := strconv.Atoi(m[2])
		e = &openresponses.Error{StatusCode: status, Type: openresponses.ErrorType(m[1]), Message: m[3]}
	}
	e.Headers = http.Header{"Retry-After": []string{"0"}}
	return e
}

// foldHash reads the request hash of the fold a compaction entry
// records, from the member agentturn writes beyond those the format
// defines. An entry without the member, or without a hash on it,
// yields "".
func foldHash(c *agentsession.CompactionEntry) string {
	raw, ok := c.Unknown[session.FoldMember]
	if !ok {
		return ""
	}
	var call session.FoldCall
	if err := json.Unmarshal(raw, &call); err != nil {
		return ""
	}
	return call.RequestHash
}

// markedItem decodes a custom entry marked with session.ResponseIDMember
// as the item the filter kept from the model that it holds, and the
// response ID the mark names, "" for an app-only input. It reports
// false for an entry without the mark, one whose mark is not a JSON
// string, or one whose data does not decode to an item of the entry's
// namespace, which is the rule agentturn/session's own transcript
// reader applies. It is the shape of the session.MarkedItem decoder
// agentturn is considering, so that the switch to the shared decoder is
// a one-line change once it is released.
func markedItem(c *agentsession.CustomEntry) (openresponses.Item, string, bool) {
	raw, ok := c.Unknown[session.ResponseIDMember]
	if !ok {
		return nil, "", false
	}
	var responseID string
	if err := json.Unmarshal(raw, &responseID); err != nil {
		return nil, "", false
	}
	item, err := openresponses.UnmarshalItem(c.Data)
	if err != nil || item.ItemType() != c.NS {
		return nil, "", false
	}
	return item, responseID, true
}

// pathTo returns the root-first path to leaf, or to the current leaf
// when leaf is empty.
func pathTo(s *agentsession.Session, leaf string) ([]agentsession.Entry, error) {
	if leaf == "" {
		leaf = s.Leaf()
	}
	if leaf == "" {
		return nil, errors.New("replay: session has no entries")
	}
	// An ID from before the 0.5 migration names the entry that carries
	// it as its legacy_id, as export.At resolves it, so a target in an
	// old report replays as it exports.
	if id, ok := s.Resolve(leaf); ok {
		leaf = id
	}
	path := s.Path(leaf)
	if path == nil {
		return nil, fmt.Errorf("replay: %w: %s", agentsession.ErrNoEntry, leaf)
	}
	return path, nil
}

// Unverifiable reports whether a strict replay of the path to leaf, or
// to the session's current leaf when leaf is empty, could check every
// call it serves. It returns nil when it could, and an error wrapping
// [ErrUnverifiable] when it could not: one naming how many responses
// are unhashed, one wrapping [ErrSubstituted] naming the env entry that
// changed workspace, one wrapping [ErrCallIDRepeated] naming the call
// ID and both its calls' entries, or those that apply joined. An error
// that does not wrap [ErrUnverifiable] is the failure to resolve leaf
// to a path at all, which says nothing either way about the record. [AllowUnhashed] and
// [AllowSubstitution] among opts leave out what they allow, as they do
// for [NewModel]; the other options are ignored.
//
// The recorder writes a response without a request hash when the path
// it wrote does not rebuild that request's input: what a compacting
// configuration whose folds were never reported produces, and what a
// transform or a hook that edits the request produces. There is
// nothing to check such a call against, so a strict replay either
// refuses it or serves it unchecked, and which of those it does is
// [AllowUnhashed].
//
// This is the rule [NewModel] applies to the path it builds, exported
// so a caller can ask before building a model or running a suite
// rather than find out at the call.
func Unverifiable(s *agentsession.Session, leaf string, opts ...Option) error {
	path, err := pathTo(s, leaf)
	if err != nil {
		return err
	}
	return unverifiable(path, apply(opts))
}

// unverifiable is [Unverifiable] over a path already in hand. Folds
// are not counted here: a compaction entry with no fold hash matters
// only if the replay folds locally, which is not known until the call,
// so serveFold decides that one.
func unverifiable(path []agentsession.Entry, o options) error {
	var errs []error
	if !o.allowUnhashed {
		errs = append(errs, unhashed(path))
	}
	if !o.allowSubst {
		errs = append(errs, substituted(path))
	}
	errs = append(errs, repeatedCallID(path))
	return errors.Join(errs...)
}

// repeatedCallID reports the first function call on path whose call ID
// an earlier call on it has.
func repeatedCallID(path []agentsession.Entry) error {
	first := map[string]string{}
	for _, e := range path {
		item, ok := e.(*agentsession.ItemEntry)
		if !ok {
			continue
		}
		call, ok := item.Item.(*openresponses.FunctionCall)
		if !ok {
			continue
		}
		if id, seen := first[call.CallID]; seen {
			return fmt.Errorf("%w: %s at %s and %s", ErrCallIDRepeated, call.CallID, id, item.ID)
		}
		first[call.CallID] = item.ID
	}
	return nil
}

// unhashed reports the responses on path that carry no request hash.
func unhashed(path []agentsession.Entry) error {
	n, responses := 0, 0
	for _, e := range path {
		r, ok := e.(*agentsession.ResponseEntry)
		if !ok {
			continue
		}
		responses++
		if r.RequestHash == "" {
			n++
		}
	}
	if n == 0 {
		return nil
	}
	return fmt.Errorf("%w: %d of %d responses on the path carry no request hash", ErrUnverifiable, n, responses)
}

// substituted reports the first env entry on path whose workspace
// differs from the one in force before it, once a response was
// recorded. Before the first env entry the workspace is absent, so a
// first env entry that names one after a response is a substitution,
// as a resume under WithEnv of a session recorded without it is.
// Before the first response nothing was recorded to hold fixed, so no
// change there is one.
func substituted(path []agentsession.Entry) error {
	var inForce *agentsession.Workspace
	responded := false
	for _, e := range path {
		switch v := e.(type) {
		case *agentsession.ResponseEntry:
			responded = true
		case *agentsession.EnvEntry:
			if responded && !agentsession.SameWorkspace(inForce, v.Workspace) {
				return fmt.Errorf("%w: env entry %s %s", ErrSubstituted, v.ID, describeChange(inForce, v.Workspace))
			}
			inForce = v.Workspace
		}
	}
	return nil
}

// describeChange says how workspace now differs from was. When kind
// and ref agree it names the members that differ, since a restart on
// the same image differs only in those, and naming kind and ref alone
// reads as a workspace compared with itself.
func describeChange(was, now *agentsession.Workspace) string {
	if was == nil || now == nil || was.Kind != now.Kind || was.Ref != now.Ref {
		return fmt.Sprintf("runs in %s where the path before it ran in %s", describeWorkspace(now), describeWorkspace(was))
	}
	keys := make([]string, 0, len(was.Unknown)+len(now.Unknown))
	for k := range was.Unknown {
		keys = append(keys, k)
	}
	for k := range now.Unknown {
		if _, ok := was.Unknown[k]; !ok {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	var diffs []string
	for _, k := range keys {
		a, inWas := was.Unknown[k]
		b, inNow := now.Unknown[k]
		switch {
		case !inWas:
			diffs = append(diffs, fmt.Sprintf("%s %s where it had none", k, b))
		case !inNow:
			diffs = append(diffs, fmt.Sprintf("no %s where it was %s", k, a))
		case !sameJSON(a, b):
			diffs = append(diffs, fmt.Sprintf("%s %s where it was %s", k, b, a))
		}
	}
	return fmt.Sprintf("runs in %s with %s", describeWorkspace(now), strings.Join(diffs, ", "))
}

// sameJSON reports whether two JSON values are equal as values, so
// spacing or key order that differs does not count.
func sameJSON(a, b json.RawMessage) bool {
	var va, vb any
	if json.Unmarshal(a, &va) != nil || json.Unmarshal(b, &vb) != nil {
		return string(a) == string(b)
	}
	return reflect.DeepEqual(va, vb)
}

func describeWorkspace(w *agentsession.Workspace) string {
	switch {
	case w == nil:
		return "no named workspace"
	case w.Ref == "":
		return w.Kind
	}
	return w.Kind + " " + w.Ref
}

// Steps returns how many recorded calls, responses, folds and failed
// attempts, the path holds.
func (m *Model) Steps() int { return len(m.steps) }

// Settings returns the settings in force at each recorded step, in
// path order and indexed as [Served.N] less one: the model,
// instructions, reasoning, text format, tools and passthrough members
// the config entries on the path say that call was made under. A
// judge, or a product checking what it sent, reads them here rather
// than replaying the config entries itself.
func (m *Model) Settings() []agentsession.Settings {
	out := make([]agentsession.Settings, len(m.steps))
	for i, st := range m.steps {
		out[i] = st.settings
	}
	return out
}

// SettingsAt returns the settings of step n, numbered as [Served.N]
// is. It reports false for a step the path does not hold.
func (m *Model) SettingsAt(n int) (agentsession.Settings, bool) {
	if n < 1 || n > len(m.steps) {
		return agentsession.Settings{}, false
	}
	return m.steps[n-1].settings, true
}

// BeforeModelCall serves the recorded settings of the call about to be
// served: it replaces the request's instructions and tool list with
// the ones in force at that response on the path. Chain it from the
// configuration under test, last, so the hook order stays the
// product's:
//
//	inner := cfg.BeforeModelCall
//	cfg.BeforeModelCall = func(ctx context.Context, req *openresponses.Request) error {
//		if inner != nil {
//			if err := inner(ctx, req); err != nil {
//				return err
//			}
//		}
//		return model.BeforeModelCall(ctx, req)
//	}
//
// Without it a strict replay measures the layers as they are today
// rather than the run: a product whose instructions are rebuilt each
// turn from a store, a skill set or a memory block diverges at the
// first call, and the error names two hashes and no layer. The other
// recorded settings are left alone, and [Model.Settings] holds them
// for a caller that wants to serve more.
func (m *Model) BeforeModelCall(_ context.Context, req *openresponses.Request) error {
	st, ok := m.peek()
	if !ok {
		return nil
	}
	req.Instructions = st.settings.Instructions
	req.Tools = append(openresponses.Tools(nil), st.settings.Tools...)
	return nil
}

// peek returns the next response step to be served, skipping the folds
// before it, which are served by the transform and not by the loop.
func (m *Model) peek() (step, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := m.next; i < len(m.steps); i++ {
		if m.steps[i].resp != nil {
			return m.steps[i], true
		}
	}
	return step{}, false
}

// Served returns how many of them have been served.
func (m *Model) Served() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.next
}

// Reset makes the model serve from the start of the path again.
func (m *Model) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.next = 0
	m.folded, m.foldedAt = "", 0
}

// take returns the next step and advances, or ErrExhausted.
func (m *Model) take() (step, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.next >= len(m.steps) {
		return step{}, m.next, fmt.Errorf("%w: %d served", ErrExhausted, m.next)
	}
	st := m.steps[m.next]
	m.next++
	return st, m.next, nil
}

// skippedFold is what a divergence at a local fold the replay did not
// make adds: from agentturn v0.0.15 compact.NewLocal skips a fold whose
// input is below its minimum, and backs off from a prefix whose fold
// failed, so a recording made before then folds where a replay under
// the same options does not.
const skippedFold = "; from agentturn v0.0.15 compact.NewLocal skips a fold whose input is below its minimum, by default the larger of an eighth of the budget and twice an empty summary's estimate, which compact.WithMinFold(0) removes, and does not fold again a prefix whose fold failed"

// isFoldRequest reports whether a request is shaped like a local
// fold's summary call: no tools and no instructions, which is what
// compact.NewLocal sends.
func isFoldRequest(req openresponses.Request) bool {
	return len(req.Tools) == 0 && req.Instructions == ""
}

// CreateStream serves the next recorded call. When that is a fold, the
// request must be a local fold's summary call and the compaction
// entry's summary is served as the answer; when it is a response, the
// response entry is served after its hash check in strict mode; when
// it is an attempt that failed, its failure is returned for the loop
// to retry, unchecked, since the record hashes only the attempt that
// answered.
func (m *Model) CreateStream(_ context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	st, n, err := m.take()
	if err != nil {
		return err
	}
	if st.retry != nil {
		got, err := agentsession.RequestHash(session.Canonical(req))
		if err != nil {
			return err
		}
		m.observe(Served{Kind: KindFailure, N: n, EntryID: st.retry.ID, Got: got})
		return serveFailure(n, st.retry.ID, st.failure)
	}
	if st.comp != nil {
		if isFoldRequest(req) {
			return m.serveFold(st, n, req, sink)
		}
		// The configuration under test sent a turn where the
		// recording folded.
		hint := ""
		if st.foldHash != "" {
			hint = skippedFold
		}
		return fmt.Errorf("%w: compaction %s (step %d): the recording folded here and the request did not%s", ErrDiverged, st.comp.ID, n, hint)
	}
	if st.failed != nil {
		if isFoldRequest(req) {
			return m.serveFailedFold(st, n, req, sink)
		}
		return fmt.Errorf("%w: failed fold %s (step %d): the recording tried to fold here and the request did not%s", ErrDiverged, st.failed.ID, n, skippedFold)
	}
	// Got is computed whether or not the entry recorded a hash to
	// compare it against: it is what this replay sent, and for a call
	// the record is silent about it is the only account of that there
	// is. Only the comparison needs a recorded hash.
	got, err := agentsession.RequestHash(session.Canonical(req))
	if err != nil {
		return err
	}
	sv := Served{Kind: KindResponse, N: n, EntryID: st.resp.ID, Recorded: st.resp.RequestHash, Got: got}
	sv.Match = sv.Recorded != "" && got == sv.Recorded
	m.observe(sv)
	// An entry that recorded no request hash is served by position: the
	// record does not say what was sent, which is not the same as
	// saying that what was sent differs, and refusing here would report
	// a divergence that was never measured. A strict model reaches this
	// only under AllowUnhashed, because NewModel refuses such a path
	// outright.
	if m.opts.strict && sv.Recorded != "" && !sv.Match {
		return fmt.Errorf("%w: response %s (step %d): recorded %s, received %s%s", ErrDiverged, st.resp.ID, n, sv.Recorded, sv.Got, m.refolded(got, n))
	}
	return serveResponse(st, n, req, sink)
}

// Create serves the next recorded call as a complete response.
func (m *Model) Create(ctx context.Context, req openresponses.Request) (*openresponses.Response, error) {
	return openresponses.CollectStream(ctx, m, req)
}

// Compact serves the next fold, which must be next on the path, as a
// compaction response carrying the recorded summary item.
//
// A fold through the compaction endpoint is served unchecked, in every
// mode, [AllowUnhashed] or not. It is the one call a strict model does
// not check and cannot: the endpoint takes a
// [openresponses.CompactRequest] rather than a request the format
// hashes, so the record holds nothing to compare it against and its
// absence is not [ErrUnverifiable] but the shape of the endpoint. The
// step order is still enforced. [Served.Recorded] carries the fold
// hash when the entry has one, which is a session recorded through a
// local fold and replayed through the endpoint; it is reported rather
// than checked, for the same reason.
func (m *Model) Compact(_ context.Context, req openresponses.CompactRequest) (*openresponses.CompactResponse, error) {
	st, n, err := m.take()
	if err != nil {
		return nil, err
	}
	switch {
	case st.retry != nil:
		return nil, fmt.Errorf("%w: model retry %s (step %d): the recording failed a model call here and the request is a compaction", ErrDiverged, st.retry.ID, n)
	case st.failed != nil:
		// The endpoint's fold failed as the call did, and is served
		// as the error its text spells, unchecked as every endpoint
		// fold is.
		m.observe(Served{Kind: KindFailedFold, N: n, EntryID: st.failed.ID, Recorded: st.foldHash})
		return nil, serveFailure(n, st.failed.ID, strings.TrimPrefix(st.fold.Error, "compact: "))
	case st.comp == nil:
		return nil, fmt.Errorf("%w: response %s (step %d): the recording made a model call here and the request is a compaction", ErrDiverged, st.resp.ID, n)
	}
	m.observe(Served{Kind: KindFold, N: n, EntryID: st.comp.ID, Recorded: st.foldHash})
	return &openresponses.CompactResponse{
		ID:     openresponses.NewID("resp"),
		Object: openresponses.ObjectCompaction,
		Output: openresponses.Items{st.summary},
		Usage:  st.comp.Usage,
	}, nil
}

func (m *Model) observe(sv Served) {
	if sv.Kind == KindFold {
		m.mu.Lock()
		m.folded, m.foldedAt = sv.Got, sv.N
		m.mu.Unlock()
	}
	if m.opts.observer != nil {
		m.opts.observer(sv)
	}
}

// refolded says, for a response step that received got, whether got is
// the request of the fold served at the step before: the loop asked
// for the summary again rather than applying the one served. From
// agentturn v0.0.13 compact.NewLocal refuses a summary no smaller than
// what it folds and asks again, and a fold recorded before then with
// such a summary is served and refused. It returns the sentence to add
// to the divergence, or "".
func (m *Model) refolded(got string, n int) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.folded == "" || m.folded != got || m.foldedAt != n-1 {
		return ""
	}
	return fmt.Sprintf("; that is the request of the fold served at step %d, sent again: the loop did not apply the summary served there, as compact.NewLocal from agentturn v0.0.13 refuses one no smaller than what it folds, which a recording made before it may have applied", n-1)
}

// serveResponse streams a recorded response: its items, complete and
// without deltas, then the terminal event the entry recorded.
func serveResponse(st step, n int, req openresponses.Request, sink openresponses.EventSink) error {
	resp := openresponses.NewResponse(req)
	resp.ID = st.resp.ResponseID
	if st.resp.Model != "" {
		resp.Model = st.resp.Model
	}
	resp.Usage = st.resp.Usage
	if st.resp.Status == openresponses.ResponseStatusFailed {
		if st.resp.Error != nil {
			return &Failure{N: n, EntryID: st.resp.ID, Err: st.resp.Error.Err(0)}
		}
		return fmt.Errorf("replay: recorded response failed at step %d (%s)", n, st.resp.ID)
	}
	em := openresponses.NewEmitter(sink, resp)
	if err := em.Start(); err != nil {
		return err
	}
	// The items are sent as the two events Emitter.Item would send,
	// and not through it, because it promotes an unset status to
	// completed as it closes an item. A server that leaves an optional
	// field unset, as Ollama does the status of a reasoning item, is
	// recorded without it, and an item served with a field the server
	// never sent changes every later request of the replayed run: the
	// diff of the two canonical requests is one line, and the
	// divergence names two hashes and no field. The emitter still owns
	// the output indices, the response snapshot and the terminal
	// event.
	for _, item := range st.output {
		idx := len(resp.Output)
		resp.Output = append(resp.Output, item)
		if err := sink.Send(&openresponses.OutputItemAddedEvent{OutputIndex: idx, Item: item}); err != nil {
			return err
		}
		if err := sink.Send(&openresponses.OutputItemDoneEvent{OutputIndex: idx, Item: item}); err != nil {
			return err
		}
	}
	if st.resp.Incomplete != nil {
		return em.Incomplete(st.resp.Incomplete.Reason)
	}
	return em.Complete()
}

// serveFold answers a local fold's summary call with the compaction
// entry's summary text, after checking the fold's own request against
// the hash the entry recorded for it.
func (m *Model) serveFold(st step, n int, req openresponses.Request, sink openresponses.EventSink) error {
	// As in CreateStream, Got is what this replay sent and is reported
	// whether or not there is a recorded hash to compare it against.
	got, err := agentsession.RequestHash(session.Canonical(req))
	if err != nil {
		return err
	}
	sv := Served{Kind: KindFold, N: n, EntryID: st.comp.ID, Recorded: st.foldHash, Got: got}
	sv.Match = sv.Recorded != "" && got == sv.Recorded
	m.observe(sv)
	// An entry that recorded no fold hash leaves the shape of a fold's
	// request as the only check there is, which is no check at all
	// against what the recording sent. It is refused here and not in
	// NewModel because reaching here is what makes the absence matter:
	// a compaction entry recorded through the endpoint also carries no
	// fold hash, and replays through Compact, which has nothing to
	// check by construction. The two are not quite indistinguishable on
	// the path — an endpoint fold recorded by agentturn v0.0.6 or later
	// writes the fold member and omits only request_hash, where a
	// recording older than that writes no member at all — but foldHash
	// reads both as "", the signal rests on a response ID the endpoint
	// is not obliged to return, and neither says which compactor this
	// replay will use. Arriving here does. AllowUnhashed is how a
	// recording made before agentturn v0.0.6 replays.
	if m.opts.strict && sv.Recorded == "" && !m.opts.allowUnhashed {
		return fmt.Errorf("%w: compaction %s (step %d): the entry records no hash for the fold's own request", ErrUnverifiable, st.comp.ID, n)
	}
	if m.opts.strict && sv.Recorded != "" && !sv.Match {
		return fmt.Errorf("%w: compaction %s (step %d): the fold's request: recorded %s, received %s%s", ErrDiverged, st.comp.ID, n, sv.Recorded, sv.Got, outputLimitOnly(req, sv.Recorded))
	}
	resp := openresponses.NewResponse(req)
	resp.Usage = st.comp.Usage
	em := openresponses.NewEmitter(sink, resp)
	msg, err := em.Message(openresponses.PhaseFinalAnswer)
	if err != nil {
		return err
	}
	if err := msg.Text(m.opts.foldText(st.summary)); err != nil {
		return err
	}
	if err := msg.Close(); err != nil {
		return err
	}
	return em.Complete()
}

// outputLimitOnly says, for a fold's request that did not match the
// hash recorded for it, whether the request without max_output_tokens
// does. agentturn v0.0.13 sets it on every local fold's summary call,
// and a recording made before it never sent one, so without this every
// fold such a recording holds diverges naming two hashes and no
// member. It returns the sentence to add to the divergence, or "".
func outputLimitOnly(req openresponses.Request, recorded string) string {
	if req.MaxOutputTokens == nil {
		return ""
	}
	req.MaxOutputTokens = nil
	got, err := agentsession.RequestHash(session.Canonical(req))
	if err != nil || got != recorded {
		return ""
	}
	return "; the request differs only in its max_output_tokens, which compact.NewLocal sets from agentturn v0.0.13 and a recording made before it never sent: compact.WithRequest can clear it"
}

// Errors a failed fold's text may begin with: the three compact.NewLocal
// reports for a summary it could not apply, after which the transcript
// is sent unfolded, and the prefix of a summary call that failed. A
// summary with no text failed the turn before agentturn v0.0.15.
var (
	foldTooLarge   = compact.ErrSummaryTooLarge.Error()
	foldIncomplete = compact.ErrSummaryIncomplete.Error()
	foldNoText     = compact.ErrSummaryNoText.Error()
	foldCallFailed = "compact: summary: "
)

// foldCalls is how many summary calls a failed fold made. A record
// before agentturn v0.0.13 counts none: a summary with no text was
// asked twice before the fold failed, and anything else once.
func foldCalls(f session.FailedFold) int {
	if f.Attempts > 0 {
		return f.Attempts
	}
	if strings.HasPrefix(f.Error, foldNoText) {
		return 2
	}
	return 1
}

// serveFailedFold answers one summary call of a fold that failed, so
// that compact.NewLocal fails it the way the record says it did. The
// record keeps the error, not the summary, so the answer is rebuilt
// from the error: a summary too large is the request's own input as
// JSON text, which compact's default estimate weighs above that input,
// so a configuration with an estimator of its own may fold where the
// recording did not and diverge at the call after; an incomplete one is
// an empty response ended incomplete with the recorded reason; one
// with no text is an empty response; and a call that failed is the
// error its text spells, as a model_retry entry's is. Every call of
// the fold is checked against the one hash the record keeps, the last
// call's: NewLocal sends the same request each time. The usage and
// response ID are served on the last call, so a record of the replay
// sums to the recording's.
func (m *Model) serveFailedFold(st step, n int, req openresponses.Request, sink openresponses.EventSink) error {
	got, err := agentsession.RequestHash(session.Canonical(req))
	if err != nil {
		return err
	}
	sv := Served{Kind: KindFailedFold, N: n, EntryID: st.failed.ID, Recorded: st.foldHash, Got: got}
	sv.Match = sv.Recorded != "" && got == sv.Recorded
	m.observe(sv)
	if m.opts.strict && sv.Recorded == "" && !m.opts.allowUnhashed {
		return fmt.Errorf("%w: failed fold %s (step %d): the entry records no hash for the fold's own request", ErrUnverifiable, st.failed.ID, n)
	}
	if m.opts.strict && sv.Recorded != "" && !sv.Match {
		return fmt.Errorf("%w: failed fold %s (step %d): the fold's request: recorded %s, received %s%s", ErrDiverged, st.failed.ID, n, sv.Recorded, sv.Got, outputLimitOnly(req, sv.Recorded))
	}
	text := st.fold.Error
	if rest, ok := strings.CutPrefix(text, foldCallFailed); ok {
		return serveFailure(n, st.failed.ID, rest)
	}
	resp := openresponses.NewResponse(req)
	if st.attempt == st.calls {
		resp.Usage = st.fold.Usage
		if st.fold.ResponseID != "" {
			resp.ID = st.fold.ResponseID
		}
	}
	em := openresponses.NewEmitter(sink, resp)
	switch {
	case strings.HasPrefix(text, foldTooLarge):
		input, err := json.Marshal(req.Input)
		if err != nil {
			return err
		}
		msg, err := em.Message(openresponses.PhaseFinalAnswer)
		if err != nil {
			return err
		}
		if err := msg.Text(string(input)); err != nil {
			return err
		}
		if err := msg.Close(); err != nil {
			return err
		}
		return em.Complete()
	case strings.HasPrefix(text, foldIncomplete):
		reason := strings.TrimPrefix(strings.TrimPrefix(text, foldIncomplete), ": ")
		if err := em.Start(); err != nil {
			return err
		}
		return em.Incomplete(openresponses.IncompleteReason(reason))
	case strings.HasPrefix(text, foldNoText):
		if err := em.Start(); err != nil {
			return err
		}
		return em.Complete()
	}
	return fmt.Errorf("replay: failed fold %s (step %d): the recorded error is not one a fold can be served as: %s", st.failed.ID, n, text)
}
