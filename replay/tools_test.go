package replay_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agenteval/replay"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/session"
	"github.com/ChristopherDavenport/openresponses"
	"github.com/ChristopherDavenport/openresponses/echo"
)

// recordedSession builds a session holding calls to upper with the
// given call IDs and arguments, each answered by the uppercased text.
func recordedSession(t *testing.T, calls ...[2]string) *agentsession.Session {
	t.Helper()
	s := agentsession.New(agentsession.Header{})
	for _, c := range calls {
		var a upperArgs
		if err := json.Unmarshal([]byte(c[1]), &a); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Append(&agentsession.ItemEntry{Item: &openresponses.FunctionCall{CallID: c[0], Name: "upper", Arguments: c[1]}, ResponseID: "resp_1"}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Append(&agentsession.ItemEntry{Item: openresponses.NewFunctionCallOutput(c[0], "recorded:"+a.Text)}); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func TestToolsServeRecordedOutputs(t *testing.T) {
	s := recordedSession(t, [2]string{"call_1", `{"text":"one"}`}, [2]string{"call_2", `{"text":"two"}`}, [2]string{"call_3", `{"text":"two"}`})
	tests := []struct {
		name    string
		strict  bool
		call    agenttool.Call
		want    string
		byID    bool
		ran     int
		wantErr error
	}{
		{name: "by id", call: agenttool.Call{ID: "call_1", Args: json.RawMessage(`{"text":"anything"}`)}, want: "recorded:one", byID: true},
		{name: "by arguments", call: agenttool.Call{ID: "call_x", Args: json.RawMessage(`{"text":"one"}`)}, want: "recorded:one"},
		{name: "by arguments canonically", call: agenttool.Call{ID: "call_x", Args: json.RawMessage("{ \"text\" :\t\"one\" }")}, want: "recorded:one"},
		{name: "unmatched runs the tool", call: agenttool.Call{ID: "call_x", Args: json.RawMessage(`{"text":"new"}`)}, want: "NEW", ran: 1},
		{name: "unmatched strict fails", strict: true, call: agenttool.Call{ID: "call_x", Args: json.RawMessage(`{"text":"new"}`)}, wantErr: replay.ErrDiverged},
		{name: "invalid json falls through", call: agenttool.Call{ID: "call_x", Args: json.RawMessage(`not json`)}, wantErr: errInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ran := 0
			var seen []replay.Served
			opts := []replay.Option{replay.WithObserver(func(sv replay.Served) { seen = append(seen, sv) })}
			if tt.strict {
				opts = append(opts, replay.Strict())
			}
			tools := replay.Tools(s, []agenttool.Tool{upperTool(&ran)}, opts...)
			res, err := tools[0].Execute(context.Background(), tt.call)
			switch {
			case tt.wantErr == errInvalid:
				if err == nil {
					t.Fatal("invalid arguments ran without error")
				}
			case tt.wantErr != nil:
				if !errors.Is(err, tt.wantErr) {
					t.Fatalf("err = %v, want %v", err, tt.wantErr)
				}
			case err != nil:
				t.Fatal(err)
			}
			if err == nil && res.Output.String() != tt.want {
				t.Errorf("output = %q, want %q", res.Output.String(), tt.want)
			}
			if ran != tt.ran {
				t.Errorf("the real tool ran %d time(s), want %d", ran, tt.ran)
			}
			if tt.want != "" && tt.ran == 0 {
				if len(seen) != 1 || seen[0].Kind != replay.KindCall || seen[0].ByID != tt.byID || seen[0].Name != "upper" {
					t.Errorf("observed = %+v", seen)
				}
			} else if len(seen) != 0 {
				t.Errorf("observed = %+v", seen)
			}
		})
	}
}

// errInvalid marks a case whose error is the tool's own.
var errInvalid = errors.New("invalid")

func TestToolsServeRepeatedCallsInOrder(t *testing.T) {
	s := recordedSession(t, [2]string{"call_1", `{"text":"same"}`}, [2]string{"call_2", `{"text":"same"}`})
	ran := 0
	tools := replay.Tools(s, []agenttool.Tool{upperTool(&ran)}, replay.Strict())
	// Two calls with fresh IDs and the same arguments take the two
	// recorded outputs in path order; a third has nothing left.
	var got []string
	for i := 0; i < 2; i++ {
		res, err := tools[0].Execute(context.Background(), agenttool.Call{ID: "new", Args: json.RawMessage(`{"text":"same"}`)})
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, res.Output.String())
	}
	if got[0] != "recorded:same" || got[1] != "recorded:same" {
		t.Errorf("outputs = %v", got)
	}
	if _, err := tools[0].Execute(context.Background(), agenttool.Call{ID: "new", Args: json.RawMessage(`{"text":"same"}`)}); !errors.Is(err, replay.ErrDiverged) {
		t.Errorf("third call: err = %v", err)
	}
	// Serving by ID takes that call out of the arguments queue too.
	tools = replay.Tools(s, []agenttool.Tool{upperTool(&ran)}, replay.Strict())
	if _, err := tools[0].Execute(context.Background(), agenttool.Call{ID: "call_2", Args: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := tools[0].Execute(context.Background(), agenttool.Call{ID: "new", Args: json.RawMessage(`{"text":"same"}`)}); err != nil {
		t.Fatal(err)
	}
	if _, err := tools[0].Execute(context.Background(), agenttool.Call{ID: "new", Args: json.RawMessage(`{"text":"same"}`)}); !errors.Is(err, replay.ErrDiverged) {
		t.Errorf("after id and arguments: err = %v", err)
	}
	if ran != 0 {
		t.Errorf("the real tool ran %d time(s)", ran)
	}
}

func TestToolsKeepTheDefinition(t *testing.T) {
	s := recordedSession(t)
	ran := 0
	real := agenttool.New("upper", "Uppercase the text", func(_ context.Context, a upperArgs) (string, error) { ran++; return a.Text, nil }, agenttool.WithSequential(), agenttool.WithStrict())
	tools := replay.Tools(s, []agenttool.Tool{real})
	got, want := agenttool.Definition(tools[0]), agenttool.Definition(real)
	g, _ := json.Marshal(got)
	w, _ := json.Marshal(want)
	if string(g) != string(w) {
		t.Errorf("definition changed:\n got %s\nwant %s", g, w)
	}
	if !agenttool.IsSequential(tools[0]) || !agenttool.IsStrict(tools[0]) {
		t.Error("sequential or strict not carried through")
	}
	// A pending call, one with no output on the path, is not served.
	if _, err := s.Append(&agentsession.ItemEntry{Item: &openresponses.FunctionCall{CallID: "pending", Name: "upper", Arguments: `{"text":"p"}`}, ResponseID: "resp_2"}); err != nil {
		t.Fatal(err)
	}
	tools = replay.Tools(s, []agenttool.Tool{real}, replay.Strict())
	if _, err := tools[0].Execute(context.Background(), agenttool.Call{ID: "pending", Args: json.RawMessage(`{"text":"p"}`)}); !errors.Is(err, replay.ErrDiverged) {
		t.Errorf("pending call: err = %v", err)
	}
}

// skillRead stands in for a Details value a wrapper acts on by its
// type, as agentkit's granting wrapper does on agentskill.Read.
type skillRead struct {
	Name string `json:"name"`
}

func (skillRead) RecordNS() string { return "test:read" }

func TestDetailsAsRefusesATypeWithNoNamespace(t *testing.T) {
	for name, fn := range map[string]func(){
		"interface":    func() { replay.DetailsAs[agenttool.Recordable]() },
		"no namespace": func() { replay.DetailsAs[noNS]() },
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if r := recover(); r == nil || !strings.Contains(fmt.Sprint(r), "DetailsAs") {
					t.Errorf("recovered %v", r)
				}
			}()
			fn()
		})
	}
}

type noNS struct{}

func (noNS) RecordNS() string { return "" }

// custom appends a custom entry, naming callID when it is not empty.
func custom(t *testing.T, s *agentsession.Session, ns, data, callID string) {
	t.Helper()
	if _, err := s.Append(&agentsession.CustomEntry{NS: ns, Data: json.RawMessage(data), CallID: callID}); err != nil {
		t.Fatal(err)
	}
}

// callItem and outputItem append one call to upper and its output.
func callItem(t *testing.T, s *agentsession.Session, callID, text string) {
	t.Helper()
	if _, err := s.Append(&agentsession.ItemEntry{Item: &openresponses.FunctionCall{CallID: callID, Name: "upper", Arguments: `{"text":"` + text + `"}`}, ResponseID: "resp_1"}); err != nil {
		t.Fatal(err)
	}
}

func outputItem(t *testing.T, s *agentsession.Session, callID, text string) {
	t.Helper()
	if _, err := s.Append(&agentsession.ItemEntry{Item: openresponses.NewFunctionCallOutput(callID, "recorded:"+text)}); err != nil {
		t.Fatal(err)
	}
}

// TestToolsServeTheRecord is issue 17: a served result carries the
// record its call's result carried as its Details, so a wrapper that
// acts on Details acts on a replayed call.
func TestToolsServeTheRecord(t *testing.T) {
	tests := []struct {
		name string
		// format is the session's, the current one when empty.
		format string
		build  func(t *testing.T, s *agentsession.Session)
		opts   []replay.Option
		// want is the served Details, nil for none.
		want    any
		wantErr bool
	}{
		{
			name: "named by call_id",
			build: func(t *testing.T, s *agentsession.Session) {
				callItem(t, s, "call_1", "one")
				custom(t, s, "test:read", `{"name":"one"}`, "call_1")
				outputItem(t, s, "call_1", "one")
			},
			want: replay.Record{NS: "test:read", Data: json.RawMessage(`{"name":"one"}`)},
		},
		{
			name: "decoded as the type asked for",
			build: func(t *testing.T, s *agentsession.Session) {
				callItem(t, s, "call_1", "one")
				custom(t, s, "test:read", `{"name":"one"}`, "call_1")
				outputItem(t, s, "call_1", "one")
			},
			opts: []replay.Option{replay.DetailsAs[skillRead]()},
			want: skillRead{Name: "one"},
		},
		{
			name: "the last before the output",
			build: func(t *testing.T, s *agentsession.Session) {
				callItem(t, s, "call_1", "one")
				custom(t, s, "test:written", `{"n":1}`, "call_1")
				custom(t, s, "test:read", `{"name":"one"}`, "call_1")
				outputItem(t, s, "call_1", "one")
				custom(t, s, "test:after", `{}`, "call_1")
			},
			want: replay.Record{NS: "test:read", Data: json.RawMessage(`{"name":"one"}`)},
		},
		{
			name:   "by position before call_id",
			format: "agentsession/0.5",
			build: func(t *testing.T, s *agentsession.Session) {
				callItem(t, s, "call_1", "one")
				custom(t, s, "test:read", `{"name":"one"}`, "")
				outputItem(t, s, "call_1", "one")
			},
			want: replay.Record{NS: "test:read", Data: json.RawMessage(`{"name":"one"}`)},
		},
		{
			name: "not by position once call_id is defined",
			build: func(t *testing.T, s *agentsession.Session) {
				callItem(t, s, "call_1", "one")
				custom(t, s, "policy:note", `{}`, "")
				outputItem(t, s, "call_1", "one")
			},
		},
		{
			name: "not a nested call's record",
			build: func(t *testing.T, s *agentsession.Session) {
				callItem(t, s, "call_1", "one")
				custom(t, s, session.NestedCallNS, `{"phase":"start","call_id":"n1","parent":"call_1"}`, "call_1")
				custom(t, s, session.NestedCallNS, `{"phase":"end","call_id":"n1","parent":"call_1"}`, "call_1")
				custom(t, s, "test:read", `{"name":"nested"}`, "call_1")
				outputItem(t, s, "call_1", "one")
			},
		},
		{
			name: "the namespace asked for over a later record",
			build: func(t *testing.T, s *agentsession.Session) {
				callItem(t, s, "call_1", "one")
				custom(t, s, "test:read", `{"name":"one"}`, "call_1")
				custom(t, s, "test:written", `{"n":1}`, "call_1")
				outputItem(t, s, "call_1", "one")
			},
			opts: []replay.Option{replay.DetailsAs[skillRead]()},
			want: skillRead{Name: "one"},
		},
		{
			name: "a pointer type",
			build: func(t *testing.T, s *agentsession.Session) {
				callItem(t, s, "call_1", "one")
				custom(t, s, "test:read", `{"name":"one"}`, "call_1")
				outputItem(t, s, "call_1", "one")
			},
			opts: []replay.Option{replay.DetailsAs[*skillRead]()},
			want: &skillRead{Name: "one"},
		},
		{
			name:   "not by position in a parallel batch",
			format: "agentsession/0.5",
			build: func(t *testing.T, s *agentsession.Session) {
				callItem(t, s, "call_1", "one")
				callItem(t, s, "call_2", "two")
				custom(t, s, "test:read", `{"name":"one"}`, "")
				outputItem(t, s, "call_1", "one")
				outputItem(t, s, "call_2", "two")
			},
		},
		{
			name: "not the recorder's own",
			build: func(t *testing.T, s *agentsession.Session) {
				callItem(t, s, "call_1", "one")
				custom(t, s, session.NestedCallNS, `{}`, "call_1")
				custom(t, s, session.ElicitationNS, `{}`, "call_1")
				outputItem(t, s, "call_1", "one")
			},
		},
		{
			name: "another call's",
			build: func(t *testing.T, s *agentsession.Session) {
				callItem(t, s, "call_1", "one")
				custom(t, s, "test:read", `{"name":"zero"}`, "call_0")
				outputItem(t, s, "call_1", "one")
			},
		},
		{
			name: "a record that does not decode",
			build: func(t *testing.T, s *agentsession.Session) {
				callItem(t, s, "call_1", "one")
				custom(t, s, "test:read", `{"name":1}`, "call_1")
				outputItem(t, s, "call_1", "one")
			},
			opts:    []replay.Option{replay.DetailsAs[skillRead]()},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := agentsession.New(agentsession.Header{Format: tt.format})
			tt.build(t, s)
			ran := 0
			tools := replay.Tools(s, []agenttool.Tool{upperTool(&ran)}, append(tt.opts, replay.Strict())...)
			res, err := tools[0].Execute(context.Background(), agenttool.Call{ID: "call_1", Args: json.RawMessage(`{"text":"one"}`)})
			if tt.wantErr {
				if err == nil {
					t.Fatal("served a record that does not decode")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if ran != 0 {
				t.Errorf("the real tool ran %d time(s)", ran)
			}
			got, _ := json.Marshal(res.Details)
			want, _ := json.Marshal(tt.want)
			if fmt.Sprintf("%T %s", res.Details, got) != fmt.Sprintf("%T %s", tt.want, want) {
				t.Errorf("details = %T %s, want %T %s", res.Details, got, tt.want, want)
			}
		})
	}
}

// reading is upper with a Recordable Details value, as a skill tool's
// read is.
type reading struct{ agenttool.Tool }

func (r reading) Execute(ctx context.Context, call agenttool.Call) (agenttool.Result, error) {
	res, err := r.Tool.Execute(ctx, call)
	var a upperArgs
	_ = json.Unmarshal(call.Args, &a)
	res.Details = skillRead{Name: a.Text}
	return res, err
}

// granting records the Details it sees by type, as a wrapper that
// grants on a skill read does.
type granting struct {
	agenttool.Tool
	granted *[]string
}

func (g granting) Execute(ctx context.Context, call agenttool.Call) (agenttool.Result, error) {
	res, err := g.Tool.Execute(ctx, call)
	if r, ok := res.Details.(skillRead); ok {
		*g.granted = append(*g.granted, r.Name)
	}
	return res, err
}

// TestReplayedWrapperActsOnDetails records a live run whose tool's
// Details are recordable and replays it under a wrapper that acts on
// them: the wrapper acts as it did live, and the replayed session holds
// the record beside the served call.
func TestReplayedWrapperActsOnDetails(t *testing.T) {
	ran := 0
	var live []string
	orig, end, err := rerun(t, fixtureConfig(&echo.Adapter{}, granting{reading{upperTool(&ran)}, &live}), "hello world")
	if err != nil || end.Reason != agentturn.ReasonDone {
		t.Fatalf("live run: %v %v", end, err)
	}
	if len(live) != 1 || ran != 1 {
		t.Fatalf("live run granted %v and ran the tool %d time(s)", live, ran)
	}
	model, err := replay.NewModel(orig, replay.Strict())
	if err != nil {
		t.Fatal(err)
	}
	var replayed []string
	tools := replay.Tools(orig, []agenttool.Tool{reading{upperTool(&ran)}}, replay.Strict(), replay.DetailsAs[skillRead]())
	s, end, err := rerun(t, fixtureConfig(model, granting{tools[0], &replayed}), "hello world")
	if err != nil || end.Reason != agentturn.ReasonDone {
		t.Fatalf("replay: %v %v", end, err)
	}
	if ran != 1 {
		t.Errorf("the replay ran the real tool")
	}
	if strings.Join(replayed, ",") != strings.Join(live, ",") {
		t.Errorf("the replay granted %v, the live run %v", replayed, live)
	}
	var records []string
	for _, e := range s.Path(s.Leaf()) {
		if c, ok := e.(*agentsession.CustomEntry); ok && c.NS == "test:read" {
			records = append(records, c.CallID+" "+string(c.Data))
		}
	}
	if len(records) != 1 || !strings.HasSuffix(records[0], `{"name":"hello world"}`) || strings.HasPrefix(records[0], " ") {
		t.Errorf("the replayed session's records: %q", records)
	}
}
