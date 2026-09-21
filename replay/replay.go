// Package replay serves a recorded session as a model and as tools, so
// hooks, transforms and fronts are tested against real traffic with
// nothing behind them.
//
// [Model] serves the session's recorded model calls in path order:
// each response entry on the path is one call, and each compaction
// entry is the fold that preceded the call after it. In strict mode a
// request whose hash differs from the one recorded on the entry about
// to be served is refused with [ErrDiverged]; in lenient mode the
// responses are served by position and the hashes are reported through
// the observer. [Tools] wraps a tool list so that a call matching a
// recorded function call returns its recorded output instead of
// running.
//
//	s, _ := store.Open(ctx, id)
//	model, _ := replay.NewModel(s, replay.Strict())
//	cfg.Model = model
//	cfg.Tools = replay.Tools(s, cfg.Tools, replay.Strict())
package replay

import (
	"context"
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
	// Recorded and Got are the request hash on the response entry and
	// the hash of the request received; a fold records no hash and
	// leaves both empty. Match reports whether they agree.
	Recorded, Got string
	Match         bool
	// CallID and Name describe a served tool call, and ByID reports
	// whether it matched on call ID rather than on name and arguments.
	CallID, Name string
	ByID         bool
}

type options struct {
	strict   bool
	leaf     string
	observer func(Served)
	foldText func(openresponses.Item) string
}

// Option configures a [Model] or [Tools].
type Option func(*options)

// Strict makes a model refuse a request whose hash differs from the
// recorded one, and tools refuse a call with no recorded output. The
// default is lenient: the model serves by position and reports the
// hashes through the observer, and an unmatched call runs the real
// tool.
func Strict() Option { return func(o *options) { o.strict = true } }

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
// named by [WithLeaf], or to the session's current leaf.
func NewModel(s *agentsession.Session, opts ...Option) (*Model, error) {
	o := apply(opts)
	path, err := pathTo(s, o.leaf)
	if err != nil {
		return nil, err
	}
	m := &Model{opts: o}
	for i, e := range path {
		switch v := e.(type) {
		case *agentsession.ResponseEntry:
			st := step{resp: v}
			// The response's own output is the item entries directly
			// before it that name its response ID, the rule
			// Session.RequestContext uses to drop them.
			for j := i - 1; j >= 0; j-- {
				item, ok := path[j].(*agentsession.ItemEntry)
				if !ok || item.ResponseID == "" || item.ResponseID != v.ResponseID {
					break
				}
				st.output = append(openresponses.Items{item.Item}, st.output...)
			}
			// Served items are cloned: the emitter promotes an item's
			// status on close, and entries must not be modified.
			st.output = st.output.Clone()
			m.steps = append(m.steps, st)
		case *agentsession.CompactionEntry:
			m.steps = append(m.steps, step{comp: v, summary: openresponses.Items{v.Summary}.Clone()[0]})
		}
	}
	return m, nil
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

// Steps returns how many recorded calls, responses and folds, the path
// holds.
func (m *Model) Steps() int { return len(m.steps) }

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
	got, err := agentsession.RequestHash(session.Canonical(req))
	if err != nil {
		return err
	}
	sv := Served{Kind: KindResponse, N: n, EntryID: st.resp.ID, Recorded: st.resp.RequestHash, Got: got, Match: got == st.resp.RequestHash}
	m.observe(sv)
	if m.opts.strict && !sv.Match {
		return fmt.Errorf("%w: response %s (step %d): recorded %s, received %s", ErrDiverged, st.resp.ID, n, st.resp.RequestHash, got)
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
	for _, item := range st.output {
		if err := em.Item(item); err != nil {
			return err
		}
	}
	if st.resp.Incomplete != nil {
		return em.Incomplete(st.resp.Incomplete.Reason)
	}
	return em.Complete()
}

// serveFold answers a local fold's summary call with the compaction
// entry's summary text.
func (m *Model) serveFold(st step, n int, req openresponses.Request, sink openresponses.EventSink) error {
	m.observe(Served{Kind: KindFold, N: n, EntryID: st.comp.ID})
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
