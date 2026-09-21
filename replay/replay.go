// Package replay serves a recorded session as a model and as tools, so
// hooks, transforms and fronts are tested against real traffic with
// nothing behind them.
//
// [Model] serves the session's recorded model calls in path order:
// each response entry on the path is one call, and each compaction
// entry is the fold that preceded the call after it. In strict mode a
// request whose hash differs from the one recorded on the entry about
// to be served is refused with [ErrDiverged], a fold's own request
// included, so a summary prompt, a budget or a filter that changed is
// not signed off as neutral; a call the record carries no hash for is
// refused with [ErrUnverifiable] rather than served unchecked, unless
// [AllowUnhashed] says to serve it. In lenient mode the calls are
// served by position and the hashes are reported through the observer.
// [Tools] wraps a tool list so that a call matching a recorded
// function call returns its recorded output instead of running.
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
	"strings"
	"sync"

	"github.com/ChristopherDavenport/agentsession"
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
// at the call; [AllowUnhashed] serves both unchecked instead.
var ErrUnverifiable = errors.New("replay: the record cannot say what was sent")

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
	// EntryID is the entry served: the response or compaction entry
	// for the model, the output's item entry for a tool call.
	EntryID string
	// Recorded and Got are the hash recorded for the call and the hash
	// of the request received: the response entry's own hash, or, for
	// a fold, the one on its compaction entry's fold member. A fold
	// through the compaction endpoint sends no request the format
	// hashes, an entry written before agentturn v0.0.6 carries no fold
	// member at all, and a response entry carries no hash when the path
	// does not rebuild that request's input; all three leave the two
	// empty and Match false, which a strict model reports only under
	// [AllowUnhashed], having otherwise refused the call. Match reports
	// whether the two agree, and is false when there was nothing to
	// compare: it is not on its own a statement that the call was
	// checked.
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
	leaf          string
	observer      func(Served)
	foldText      func(openresponses.Item) string
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
// The default is lenient: the model serves by position and reports the
// hashes through the observer, and an unmatched call runs the real
// tool.
func Strict() Option { return func(o *options) { o.strict = true } }

// AllowUnhashed lets a strict model serve a call whose entry recorded
// no hash: by position, and unchecked. It turns the guarantee off for
// those calls rather than relaxing it. What the caller gives up is the
// whole of what [Strict] is for — that every served call was checked
// against the request the recording made — for every call the record
// is silent about, and nothing in the replay's result distinguishes a
// call that was checked and matched from one that was never checked.
// Only [Model.Settings] and an observer subscribing to [Served] can
// tell them apart afterwards.
//
// It exists for recordings the format cannot describe: one made before
// agentturn v0.0.6, which wrote no fold member, and one whose requests
// a transform or a hook edited. Reach for it to replay an old session
// at all, not to quiet a failure on a current one.
func AllowUnhashed() Option { return func(o *options) { o.allowUnhashed = true } }

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
// items it produced, or a fold with its summary.
type step struct {
	resp    *agentsession.ResponseEntry
	output  openresponses.Items
	comp    *agentsession.CompactionEntry
	summary openresponses.Item
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
}

// NewModel builds a model over the path from the root to the leaf
// named by [WithLeaf], or to the session's current leaf. A strict
// model over a path a strict replay could not check is refused here
// with [ErrUnverifiable], rather than at the call it could not check:
// see [Unverifiable], and [AllowUnhashed] to serve it anyway.
func NewModel(s *agentsession.Session, opts ...Option) (*Model, error) {
	o := apply(opts)
	path, err := pathTo(s, o.leaf)
	if err != nil {
		return nil, err
	}
	if o.strict && !o.allowUnhashed {
		if err := unverifiable(path); err != nil {
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
			// that name its response ID: entries that are not item
			// entries are skipped and the walk stops at the first item
			// entry belonging to something else. It is the rule
			// Session.RequestContext uses to drop them, and the two
			// must agree or a replay serves an input item as output.
			// Skipping rather than stopping is what lets an observer
			// write a custom entry between two output items of one
			// response, which is where every layer above the loop puts
			// its verdict, without losing the item after it.
			if v.ResponseID != "" {
				for j := i - 1; j >= 0; j-- {
					item, ok := path[j].(*agentsession.ItemEntry)
					if !ok {
						continue
					}
					if item.ResponseID != v.ResponseID {
						break
					}
					st.output = append(openresponses.Items{item.Item}, st.output...)
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
		}
	}
	return m, nil
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

// pathTo returns the root-first path to leaf, or to the current leaf
// when leaf is empty.
func pathTo(s *agentsession.Session, leaf string) ([]agentsession.Entry, error) {
	if leaf == "" {
		leaf = s.Leaf()
	}
	if leaf == "" {
		return nil, errors.New("replay: session has no entries")
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
// [ErrUnverifiable] naming how many responses are unhashed when it
// could not.
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
func Unverifiable(s *agentsession.Session, leaf string) error {
	path, err := pathTo(s, leaf)
	if err != nil {
		return err
	}
	return unverifiable(path)
}

// unverifiable is [Unverifiable] over a path already in hand. Folds
// are not counted here: a compaction entry with no fold hash matters
// only if the replay folds locally, which is not known until the call,
// so serveFold decides that one.
func unverifiable(path []agentsession.Entry) error {
	unhashed, responses := 0, 0
	for _, e := range path {
		r, ok := e.(*agentsession.ResponseEntry)
		if !ok {
			continue
		}
		responses++
		if r.RequestHash == "" {
			unhashed++
		}
	}
	if unhashed == 0 {
		return nil
	}
	return fmt.Errorf("%w: %d of %d responses on the path carry no request hash", ErrUnverifiable, unhashed, responses)
}

// Steps returns how many recorded calls, responses and folds, the path
// holds.
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

// isFoldRequest reports whether a request is shaped like a local
// fold's summary call: no tools and no instructions, which is what
// compact.NewLocal sends.
func isFoldRequest(req openresponses.Request) bool {
	return len(req.Tools) == 0 && req.Instructions == ""
}

// CreateStream serves the next recorded call. When that is a fold, the
// request must be a local fold's summary call and the compaction
// entry's summary is served as the answer; when it is a response, the
// response entry is served after its hash check in strict mode.
func (m *Model) CreateStream(_ context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	st, n, err := m.take()
	if err != nil {
		return err
	}
	if st.comp != nil {
		if isFoldRequest(req) {
			return m.serveFold(st, n, req, sink)
		}
		// The configuration under test sent a turn where the
		// recording folded.
		return fmt.Errorf("%w: compaction %s (step %d): the recording folded here and the request did not", ErrDiverged, st.comp.ID, n)
	}
	sv := Served{Kind: KindResponse, N: n, EntryID: st.resp.ID, Recorded: st.resp.RequestHash}
	if st.resp.RequestHash != "" {
		got, err := agentsession.RequestHash(session.Canonical(req))
		if err != nil {
			return err
		}
		sv.Got, sv.Match = got, got == st.resp.RequestHash
	}
	m.observe(sv)
	// An entry that recorded no request hash is served by position: the
	// record does not say what was sent, which is not the same as
	// saying that what was sent differs, and refusing here would report
	// a divergence that was never measured. A strict model reaches this
	// only under AllowUnhashed, because NewModel refuses such a path
	// outright.
	if m.opts.strict && sv.Recorded != "" && !sv.Match {
		return fmt.Errorf("%w: response %s (step %d): recorded %s, received %s", ErrDiverged, st.resp.ID, n, sv.Recorded, sv.Got)
	}
	return serveResponse(st, req, sink)
}

// Create serves the next recorded call as a complete response.
func (m *Model) Create(ctx context.Context, req openresponses.Request) (*openresponses.Response, error) {
	return openresponses.CollectStream(ctx, m, req)
}

// Compact serves the next fold, which must be next on the path, as a
// compaction response carrying the recorded summary item.
func (m *Model) Compact(_ context.Context, req openresponses.CompactRequest) (*openresponses.CompactResponse, error) {
	st, n, err := m.take()
	if err != nil {
		return nil, err
	}
	if st.comp == nil {
		return nil, fmt.Errorf("%w: response %s (step %d): the recording made a model call here and the request is a compaction", ErrDiverged, st.resp.ID, n)
	}
	m.observe(Served{Kind: KindFold, N: n, EntryID: st.comp.ID})
	return &openresponses.CompactResponse{
		ID:     openresponses.NewID("resp"),
		Object: openresponses.ObjectCompaction,
		Output: openresponses.Items{st.summary},
		Usage:  st.comp.Usage,
	}, nil
}

func (m *Model) observe(sv Served) {
	if m.opts.observer != nil {
		m.opts.observer(sv)
	}
}

// serveResponse streams a recorded response: its items, complete and
// without deltas, then the terminal event the entry recorded.
func serveResponse(st step, req openresponses.Request, sink openresponses.EventSink) error {
	resp := openresponses.NewResponse(req)
	resp.ID = st.resp.ResponseID
	if st.resp.Model != "" {
		resp.Model = st.resp.Model
	}
	resp.Usage = st.resp.Usage
	if st.resp.Status == openresponses.ResponseStatusFailed {
		if st.resp.Error != nil {
			return st.resp.Error.Err(0)
		}
		return errors.New("replay: recorded response failed")
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
	sv := Served{Kind: KindFold, N: n, EntryID: st.comp.ID, Recorded: st.foldHash}
	if st.foldHash != "" {
		got, err := agentsession.RequestHash(session.Canonical(req))
		if err != nil {
			return err
		}
		sv.Got, sv.Match = got, got == st.foldHash
	}
	m.observe(sv)
	// An entry that recorded no fold hash leaves the shape of a fold's
	// request as the only check there is, which is no check at all
	// against what the recording sent. NewModel cannot refuse it,
	// because a compaction entry recorded through the endpoint carries
	// no fold hash and replays through Compact without ever reaching
	// here; so it is refused at the call, and AllowUnhashed is how a
	// recording made before agentturn v0.0.6 replays.
	if m.opts.strict && sv.Recorded == "" && !m.opts.allowUnhashed {
		return fmt.Errorf("%w: compaction %s (step %d): the entry records no hash for the fold's own request", ErrUnverifiable, st.comp.ID, n)
	}
	if m.opts.strict && sv.Recorded != "" && !sv.Match {
		return fmt.Errorf("%w: compaction %s (step %d): the fold's request: recorded %s, received %s", ErrDiverged, st.comp.ID, n, sv.Recorded, sv.Got)
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
