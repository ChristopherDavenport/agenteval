package replay

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
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
	// records are the records beside the call that may be its
	// result's, in path order.
	records []*agentsession.CustomEntry
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
// the live tool returned. A call with records in several namespaces is
// served the last one in a namespace given here. The namespace is the
// one T reports for its zero value, or for a new value when T is a
// pointer; it panics when T is an interface or reports no namespace,
// since it would match nothing. Several may be given, one per
// namespace. A record that does not decode as a T fails the call. A
// recorder on the replayed run writes the T as json.Marshal encodes
// it, which may differ from the recorded bytes in members T does not
// hold.
func DetailsAs[T agenttool.Recordable]() Option {
	ns := recordNS[T]()
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

// recordNS returns the namespace T reports, from a new value when T is
// a pointer so a method with a value receiver is not called on nil.
func recordNS[T agenttool.Recordable]() string {
	t := reflect.TypeFor[T]()
	var r agenttool.Recordable
	switch t.Kind() {
	case reflect.Interface:
		panic(fmt.Sprintf("replay: DetailsAs[%s]: an interface names no record namespace", t))
	case reflect.Pointer:
		r = reflect.New(t.Elem()).Interface().(agenttool.Recordable)
	default:
		var zero T
		r = zero
	}
	ns := r.RecordNS()
	if ns == "" {
		panic(fmt.Sprintf("replay: DetailsAs[%s]: the type reports no record namespace", t))
	}
	return ns
}

// harnessNS is the prefix of the namespaces agentturn's recorder
// writes for itself, a nested call or an elicitation among them. A
// record under it is not a tool's Details.
const harnessNS = "agentturn:"

// nestedCallNS is the namespace agentturn's recorder writes a nested
// call's start and end under. A nested call's own record follows its
// end directly and names the call whose tool made it, so it is not
// that call's result's.
const nestedCallNS = harnessNS + "nested_call"

// namesCalls reports whether a session's format defines call_id on a
// custom entry, from agentsession/0.6 on. Before it, a record names no
// call and is taken by position; from it, a record that names none
// belongs to none.
func namesCalls(format string) bool {
	v, ok := strings.CutPrefix(format, "agentsession/")
	if !ok {
		return true
	}
	major, minor, ok := strings.Cut(v, ".")
	if !ok {
		return true
	}
	ma, err1 := strconv.Atoi(major)
	mi, err2 := strconv.Atoi(minor)
	if err1 != nil || err2 != nil {
		return true
	}
	return ma > 0 || mi >= 6
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
// the real tool, or fails with [ErrDiverged] when Strict. Each tool is
// wrapped with [agenttool.Wrap], so it is the original in every way but
// Execute: its name, description and schema, and every property it
// declares, confinement, resource, annotations and replay safety among
// them, so a policy decides a replayed call as it decided the live one.
//
// A served result carries the record its call's result carried as its
// Details, as a [Record] or as the type [DetailsAs] names, so a wrapper
// that acts on Details acts on a served call as it did on the live one.
// The record is the last custom entry naming the call in its call_id
// before the call's output, outside agentturn's own namespaces and
// other than a nested call's record, or the last in a namespace
// [DetailsAs] names when there is one. A record the tool wrote while it
// ran, through agenttool.WriteRecord, names the call too and cannot be
// told from its result's; a tool whose Details are not recordable and
// that wrote one is served that one. A session written before
// agentsession/0.6, which defined call_id, names no call; there the
// record is taken by position, when one call alone was waiting for its
// output, so a call of a parallel batch, or any call after one that
// never got an output, is served without one.
// A session with no path to the leaf named holds no recordings, so
// every call falls through, or fails when Strict.
func Tools(s *agentsession.Session, tools []agenttool.Tool, opts ...Option) []agenttool.Tool {
	o := apply(opts)
	rec := &recording{opts: o, byID: map[string]*recorded{}, byArgs: map[string][]*recorded{}}
	path, err := pathTo(s, o.leaf)
	if err == nil {
		rec.index(path, s.Header().Format)
	}
	out := make([]agenttool.Tool, len(tools))
	for i, t := range tools {
		out[i] = agenttool.Wrap(t, func(ctx context.Context, call agenttool.Call) (agenttool.Result, error) {
			return rec.serve(ctx, t, call)
		})
	}
	return out
}

// index collects every function call with an output on the path, and
// the records beside each, in a session of the given format.
func (r *recording) index(path []agentsession.Entry, format string) {
	calls := map[string]*recorded{}
	var order []*recorded
	byPosition := !namesCalls(format)
	// waiting holds the calls with no output yet.
	waiting := map[string]*recorded{}
	// nestedEnd is the call whose nested call's end was the previous
	// entry, whose record, if one follows, is the nested call's.
	nestedEnd := ""
	for _, e := range path {
		if rec, ok := e.(*agentsession.CustomEntry); ok {
			after := nestedEnd
			nestedEnd = ""
			if rec.NS == nestedCallNS {
				var n struct {
					Phase string `json:"phase"`
				}
				if json.Unmarshal(rec.Data, &n) == nil && n.Phase == string(agentsession.RunEnd) {
					nestedEnd = rec.CallID
				}
				continue
			}
			if strings.HasPrefix(rec.NS, harnessNS) {
				continue
			}
			switch {
			case rec.CallID != "":
				if c, ok := waiting[rec.CallID]; ok && rec.CallID != after {
					c.records = append(c.records, rec)
				}
			case byPosition && len(waiting) == 1:
				for _, c := range waiting {
					c.records = append(c.records, rec)
				}
			}
			continue
		}
		nestedEnd = ""
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

// serve answers a call of t from the recording: the recorded output for
// a matching call, else t's own Execute, or a failure when strict.
func (r *recording) serve(ctx context.Context, t agenttool.Tool, call agenttool.Call) (agenttool.Result, error) {
	c, byID, ok := r.lookup(t.Name(), call.ID, call.Args)
	if !ok {
		if r.opts.strict {
			return agenttool.Result{}, fmt.Errorf("%w: no recorded output for call %s to %s with arguments %s", ErrDiverged, call.ID, t.Name(), call.Args)
		}
		return t.Execute(ctx, call)
	}
	if r.opts.observer != nil {
		r.opts.observer(Served{Kind: KindCall, N: r.count(), EntryID: c.entryID, CallID: c.call.CallID, Name: t.Name(), ByID: byID, Match: true})
	}
	res := agenttool.Result{Output: c.output.Output}
	if len(c.records) == 0 {
		return res, nil
	}
	for i := len(c.records) - 1; i >= 0; i-- {
		rec := c.records[i]
		decode, ok := r.opts.details[rec.NS]
		if !ok {
			continue
		}
		v, err := decode(rec.Data)
		if err != nil {
			return agenttool.Result{}, fmt.Errorf("replay: call %s to %s: record %s: %w", c.call.CallID, t.Name(), rec.NS, err)
		}
		res.Details = v
		return res, nil
	}
	last := c.records[len(c.records)-1]
	res.Details = Record{NS: last.NS, Data: last.Data}
	return res, nil
}

var _ agenttool.Recordable = Record{}
