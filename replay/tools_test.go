package replay_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/ChristopherDavenport/agenteval/replay"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/openresponses"
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
