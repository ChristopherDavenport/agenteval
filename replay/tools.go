package replay

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/openresponses"
)

// recorded is one function call on the path with its output and the
// record its tool's result carried, when it carried one.
type recorded struct {
	entryID string
	call    *openresponses.FunctionCall
	output  *openresponses.FunctionCallOutput
	record  *agentsession.CustomEntry
}

// Record is the Details of a served result whose recorded call carried
// a record: the namespace and data of the custom entry the recorder
// wrote beside the call from the result's [agenttool.Recordable]
// Details. It is itself Recordable, marshalling to the data as
// recorded, so a recorder on the replayed run writes the same record
// beside the served call. A wrapper that wants its own type back asks
// for it with [DetailsAs].
type Record struct {
	NS   string
	Data json.RawMessage
}

// RecordNS returns the namespace the recorded tool chose.
func (r Record) RecordNS() string { return r.NS }

// MarshalJSON returns the data as recorded.
func (r Record) MarshalJSON() ([]byte, error) {
	if len(r.Data) == 0 {
		return []byte("null"), nil
	}
	return r.Data, nil
}

// DetailsAs serves the record of a call in T's namespace as a T,
// decoded from its JSON, where it would be a [Record]: a wrapper that
// acts on a result's Details by its type, such as one that grants a
// skill's tools when the Details are the skill's read, then sees what
// the live tool returned. The namespace is the one T's zero value
// reports. Several may be given, one per namespace. A record that does
// not decode as a T fails the call.
func DetailsAs[T agenttool.Recordable]() Option {
	var zero T
	ns := zero.RecordNS()
	return func(o *options) {
		if o.details == nil {
			o.details = map[string]func(json.RawMessage) (any, error){}
		}
		o.details[ns] = func(data json.RawMessage) (any, error) {
			var v T
			err := json.Unmarshal(data, &v)
			return v, err
		}
	}
}

// harnessNS is the prefix of the namespaces agentturn's recorder
// writes for itself, a nested call or an elicitation among them. A
// record under it is not a tool's Details.
const harnessNS = "agentturn:"

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
//
// A served result carries the record its call's result carried as its
// Details, as a [Record] or as the type [DetailsAs] names, so a wrapper
// that acts on Details acts on a served call as it did on the live one.
// The record is the last custom entry naming the call in its call_id
// before the call's output, outside agentturn's own namespaces. A
// session written before call_id was defined names no call; there the
// record is taken by position, when one call alone was waiting for its
// output, and a call of a parallel batch is served without one.
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
	// waiting holds the calls with no output yet, for a record that
	// names no call.
	waiting := map[string]*recorded{}
	for _, e := range path {
		if rec, ok := e.(*agentsession.CustomEntry); ok {
			if strings.HasPrefix(rec.NS, harnessNS) {
				continue
			}
			if rec.CallID != "" {
				if c, ok := waiting[rec.CallID]; ok {
					c.record = rec
				}
				continue
			}
			if len(waiting) == 1 {
				for _, c := range waiting {
					c.record = rec
				}
			}
			continue
		}
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
			waiting[v.CallID] = c
			order = append(order, c)
		case *openresponses.FunctionCallOutput:
			if c, ok := calls[v.CallID]; ok && c.output == nil {
				c.output = v
				c.entryID = item.ID
				delete(waiting, v.CallID)
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
	res := agenttool.Result{Output: c.output.Output}
	if c.record != nil {
		res.Details = Record{NS: c.record.NS, Data: c.record.Data}
		if decode, ok := t.rec.opts.details[c.record.NS]; ok {
			v, err := decode(c.record.Data)
			if err != nil {
				return agenttool.Result{}, fmt.Errorf("replay: call %s to %s: record %s: %w", c.call.CallID, t.Name(), c.record.NS, err)
			}
			res.Details = v
		}
	}
	return res, nil
}

// Sequential reports the wrapped tool's answer.
func (t *replayed) Sequential() bool { return agenttool.IsSequential(t.Tool) }

// Strict reports the wrapped tool's answer.
func (t *replayed) Strict() bool { return agenttool.IsStrict(t.Tool) }

var (
	_ agenttool.Tool       = (*replayed)(nil)
	_ agenttool.Sequential = (*replayed)(nil)
	_ agenttool.Strict     = (*replayed)(nil)

	_ agenttool.Recordable = Record{}
)
