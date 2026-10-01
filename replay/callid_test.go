package replay_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agenteval/replay"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agenttool"
)

// legacySession reads a session of format agentsession/0.8 whose
// entries are members, each chained to the one before it: what a
// recorder before agentturn v0.0.12 wrote, which Append no longer
// writes when a call ID repeats.
func legacySession(t *testing.T, entries ...map[string]any) (*agentsession.Session, []string) {
	t.Helper()
	var b strings.Builder
	b.WriteString(`{"type":"session","format":"agentsession/0.8","id":"01995b2a-0000-7000-8000-000000000009","created_at":"2026-09-20T13:00:00Z","payload":"openresponses/2026-04-24"}` + "\n")
	var ids []string
	var parent any
	for _, e := range entries {
		e["parent"] = parent
		e["ts"] = "2026-09-20T12:00:00Z"
		data, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		id, _, err := agentsession.EntryHashes(data)
		if err != nil {
			t.Fatal(err)
		}
		e["id"] = id
		if data, err = json.Marshal(e); err != nil {
			t.Fatal(err)
		}
		b.Write(data)
		b.WriteByte('\n')
		ids = append(ids, id)
		parent = id
	}
	s, err := agentsession.Read(strings.NewReader(b.String()))
	if err != nil {
		t.Fatal(err)
	}
	return s, ids
}

func legacyCall(resp, text string) map[string]any {
	return map[string]any{"type": "item", "response": resp, "item": map[string]any{"type": "function_call", "call_id": "call_0", "name": "upper", "arguments": `{"text":"` + text + `"}`}}
}

func legacyOutput(text string) map[string]any {
	return map[string]any{"type": "item", "item": map[string]any{"type": "function_call_output", "call_id": "call_0", "output": "recorded:" + text}}
}

// repeatedCallIDs is a session whose provider numbered its calls per
// response: call_0 twice, each answered.
func repeatedCallIDs(t *testing.T) (*agentsession.Session, []string) {
	return legacySession(t, legacyCall("resp_1", "one"), legacyOutput("one"), legacyCall("resp_2", "two"), legacyOutput("two"))
}

// Issue 33: each call is served its own output, the repeat under the
// ID the loop now gives it, and neither runs its tool.
func TestToolsServeARepeatedCallID(t *testing.T) {
	s, _ := repeatedCallIDs(t)
	tests := []struct {
		name   string
		strict bool
	}{
		{"lenient", false},
		{"strict", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var opts []replay.Option
			if tt.strict {
				opts = append(opts, replay.Strict())
			}
			ran := 0
			tools := replay.Tools(s, []agenttool.Tool{upperTool(&ran)}, opts...)
			calls := []struct{ id, text string }{{"call_0", "one"}, {"call_0_1", "two"}}
			for _, c := range calls {
				res, err := tools[0].Execute(context.Background(), agenttool.Call{ID: c.id, Args: json.RawMessage(`{"text":"` + c.text + `"}`)})
				if err != nil {
					t.Fatalf("%s: %v", c.id, err)
				}
				if want := "recorded:" + c.text; res.Output.String() != want {
					t.Errorf("%s: output %q, want %q", c.id, res.Output.String(), want)
				}
			}
			if ran != 0 {
				t.Errorf("the real tool ran %d time(s)", ran)
			}
		})
	}
}

// Issue 33: a strict replay of such a path is refused up front, naming
// the call ID and both calls, since the loop renames the repeat and
// the request after it cannot hash to the recorded one.
func TestStrictRefusesARepeatedCallID(t *testing.T) {
	s, ids := repeatedCallIDs(t)
	err := replay.Unverifiable(s, "")
	if !errors.Is(err, replay.ErrCallIDRepeated) || !errors.Is(err, replay.ErrUnverifiable) {
		t.Fatalf("Unverifiable: err = %v, want ErrCallIDRepeated", err)
	}
	for _, want := range []string{"call_0", ids[0], ids[2]} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name %s: %v", want, err)
		}
	}
	if _, err := replay.NewModel(s, replay.Strict(), replay.AllowUnhashed(), replay.AllowSubstitution()); !errors.Is(err, replay.ErrCallIDRepeated) {
		t.Errorf("strict NewModel: err = %v, want ErrCallIDRepeated", err)
	}
	if _, err := replay.NewModel(s); err != nil {
		t.Errorf("lenient NewModel: %v", err)
	}
}
