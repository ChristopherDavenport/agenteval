package replay

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/openresponses"
)

// recorded is one function call on the path with its output.
type recorded struct {
	entryID string
	call    *openresponses.FunctionCall
	output  *openresponses.FunctionCallOutput
}

// recording indexes the calls on a path by call ID and by name and
// canonical arguments. Calls with the same name and arguments are
// served in path order.
type recording struct {
	opts   options
	byID   map[string]*recorded
	byArgs map[string][]*recorded

	mu     sync.Mutex
	served int
}

// Tools wraps tools so that a call whose call_id, or whose name and
// arguments, match a function call with an output on the path returns
// that output instead of running. Arguments are compared canonically,
// RFC 8785 over the JSON, the rule the request hash uses, so a
// serialiser that reorders members still matches; arguments that are
// not JSON compare byte for byte. A call with no recorded output runs
// the real tool, or fails with [ErrDiverged] when Strict. A tool's
// name, description, schema, strictness and sequencing are unchanged.
// A session with no path to the leaf named holds no recordings, so
// every call falls through, or fails when Strict.
func Tools(s *agentsession.Session, tools []agenttool.Tool, opts ...Option) []agenttool.Tool {
	o := apply(opts)
	rec := &recording{opts: o, byID: map[string]*recorded{}, byArgs: map[string][]*recorded{}}
	path, err := pathTo(s, o.leaf)
	if err == nil {
		rec.index(path)
	}
	out := make([]agenttool.Tool, len(tools))
	for i, t := range tools {
		out[i] = &replayed{Tool: t, rec: rec}
	}
	return out
}

// index collects every function call with an output on the path.
func (r *recording) index(path []agentsession.Entry) {
	calls := map[string]*recorded{}
	var order []*recorded
	for _, e := range path {
		item, ok := e.(*agentsession.ItemEntry)
		if !ok {
			continue
		}
		switch v := item.Item.(type) {
		case *openresponses.FunctionCall:
			if _, seen := calls[v.CallID]; seen {
				continue
			}
			c := &recorded{call: v}
			calls[v.CallID] = c
			order = append(order, c)
		case *openresponses.FunctionCallOutput:
			if c, ok := calls[v.CallID]; ok && c.output == nil {
				c.output = v
				c.entryID = item.ID
			}
		}
	}
	for _, c := range order {
		if c.output == nil {
			continue
		}
		r.byID[c.call.CallID] = c
		key := argsKey(c.call.Name, c.call.Arguments)
		r.byArgs[key] = append(r.byArgs[key], c)
	}
}

// argsKey is the lookup key for a call by name and arguments: the
// canonical hash of the arguments when they are JSON, else the hash of
// the bytes.
func argsKey(name, args string) string {
	if json.Valid([]byte(args)) {
		if h, err := agentsession.HashRequestJSON([]byte(args)); err == nil {
			return name + "\x00" + h
		}
	}
	return name + "\x00" + agentsession.HashBytes([]byte(args))
}

// lookup finds the recorded output for a call, by ID first and then
// by name and arguments, taking the next unserved match.
func (r *recording) lookup(name, callID string, args json.RawMessage) (*recorded, bool, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c, ok := r.byID[callID]; ok && c.call.Name == name {
		delete(r.byID, callID)
		r.unqueue(c)
		return c, true, true
	}
	key := argsKey(name, string(args))
	queue := r.byArgs[key]
	if len(queue) == 0 {
		return nil, false, false
	}
	c := queue[0]
	r.byArgs[key] = queue[1:]
	delete(r.byID, c.call.CallID)
	return c, false, true
}

// unqueue removes c from its arguments queue once it was served by ID.
func (r *recording) unqueue(c *recorded) {
	key := argsKey(c.call.Name, c.call.Arguments)
	queue := r.byArgs[key]
	for i, q := range queue {
		if q == c {
			r.byArgs[key] = append(queue[:i:i], queue[i+1:]...)
			return
		}
	}
}

func (r *recording) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.served++
	return r.served
}

// replayed is a tool whose calls are answered from the recording when
// they match.
type replayed struct {
	agenttool.Tool
	rec *recording
}

// Execute serves the recorded output for a matching call, else runs the
// wrapped tool or fails when strict.
func (t *replayed) Execute(ctx context.Context, call agenttool.Call) (agenttool.Result, error) {
	c, byID, ok := t.rec.lookup(t.Name(), call.ID, call.Args)
	if !ok {
		if t.rec.opts.strict {
			return agenttool.Result{}, fmt.Errorf("%w: no recorded output for call %s to %s with arguments %s", ErrDiverged, call.ID, t.Name(), call.Args)
		}
		return t.Tool.Execute(ctx, call)
	}
	if t.rec.opts.observer != nil {
		t.rec.opts.observer(Served{Kind: KindCall, N: t.rec.count(), EntryID: c.entryID, CallID: c.call.CallID, Name: t.Name(), ByID: byID, Match: true})
	}
	return agenttool.Result{Output: c.output.Output}, nil
}

// Sequential reports the wrapped tool's answer.
func (t *replayed) Sequential() bool { return agenttool.IsSequential(t.Tool) }

// Strict reports the wrapped tool's answer.
func (t *replayed) Strict() bool { return agenttool.IsStrict(t.Tool) }

var (
	_ agenttool.Tool       = (*replayed)(nil)
	_ agenttool.Sequential = (*replayed)(nil)
	_ agenttool.Strict     = (*replayed)(nil)
)
