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
	"github.com/ChristopherDavenport/openresponses"
)

// responseIDs returns the entry IDs of the responses on the path to
// the session's leaf, in order.
func responseIDs(s *agentsession.Session) []string {
	var out []string
	for _, e := range s.Path(s.Leaf()) {
		if r, ok := e.(*agentsession.ResponseEntry); ok {
			out = append(out, r.ID)
		}
	}
	return out
}

// Issue 45: From serves the steps after an entry on the path, AfterBase
// those after a fork's base, and the settings still accumulate from
// the whole path.
func TestFromServesTheStepsAfterTheEntry(t *testing.T) {
	multi := loadFixture(t, "multi")
	responses := responseIDs(multi)
	if len(responses) != 2 {
		t.Fatalf("the multi fixture holds %d responses, want 2", len(responses))
	}
	// A fork at the first response, with one exchange of its own.
	fork, err := agentsession.Fork(multi, responses[0], agentsession.Header{})
	if err != nil {
		t.Fatal(err)
	}
	if fork.Header().Base != responses[0] {
		t.Fatalf("the fork's base is %q, want %s", fork.Header().Base, responses[0])
	}
	for _, e := range []agentsession.Entry{
		agentsession.NewItemEntry(openresponses.UserText("more")),
		&agentsession.ResponseEntry{ResponseID: "resp_f", Model: "echo/echo-1", Status: openresponses.ResponseStatusCompleted, RequestHash: "sha256:" + strings.Repeat("0", 64)},
	} {
		if _, err := fork.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	tests := []struct {
		name    string
		s       *agentsession.Session
		opts    []replay.Option
		steps   int
		wantErr string
	}{
		{"the whole path", multi, nil, 2, ""},
		{"from the first response", multi, []replay.Option{replay.From(responses[0])}, 1, ""},
		{"from the last response", multi, []replay.Option{replay.From(responses[1])}, 0, ""},
		{"from the root", multi, []replay.Option{replay.From(multi.Path(multi.Leaf())[0].Base().ID)}, 2, ""},
		{"from an entry not on the path", multi, []replay.Option{replay.From("sha256:nope")}, 0, "not on the path"},
		{"after the base of a fork", fork, []replay.Option{replay.AfterBase()}, 1, ""},
		{"after the base of a session with none", multi, []replay.Option{replay.AfterBase()}, 0, "names no base"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model, err := replay.NewModel(tt.s, append([]replay.Option{replay.Strict()}, tt.opts...)...)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("NewModel = %v, want an error saying %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if model.Steps() != tt.steps {
				t.Errorf("Steps = %d, want %d", model.Steps(), tt.steps)
			}
			// The config entry before the base is in force at the steps
			// served after it.
			for i, st := range model.Settings() {
				if st.Model != "echo/echo-1" {
					t.Errorf("step %d settings model = %q, want the config entry's", i+1, st.Model)
				}
			}
		})
	}
}

// Issue 45: a strict model counts the unhashed responses among the
// steps it serves, so a fork whose base holds an unhashed response
// replays after the base; the other checks stay over the whole path.
func TestFromCountsUnhashedAmongTheStepsServed(t *testing.T) {
	s := agentsession.New(agentsession.Header{})
	req := openresponses.Request{Model: "m", Input: openresponses.Items{openresponses.UserText("hi")}}
	cfg, err := agentsession.ConfigFromRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	entries := []agentsession.Entry{
		cfg,
		agentsession.NewItemEntry(openresponses.UserText("hi")),
		&agentsession.ResponseEntry{ResponseID: "resp_1", Model: "m", Status: openresponses.ResponseStatusCompleted},
		agentsession.NewItemEntry(openresponses.UserText("again")),
		&agentsession.ResponseEntry{ResponseID: "resp_2", Model: "m", Status: openresponses.ResponseStatusCompleted, RequestHash: "sha256:" + strings.Repeat("0", 64)},
	}
	var first string
	for i, e := range entries {
		id, err := s.Append(e)
		if err != nil {
			t.Fatal(err)
		}
		if i == 2 {
			first = id
		}
	}
	if err := replay.Unverifiable(s, ""); !errors.Is(err, replay.ErrUnverifiable) {
		t.Fatalf("Unverifiable over the whole path = %v, want ErrUnverifiable", err)
	}
	if err := replay.Unverifiable(s, "", replay.From(first)); err != nil {
		t.Errorf("Unverifiable from the unhashed response = %v, want nil", err)
	}
	if _, err := replay.NewModel(s, replay.Strict(), replay.From(first)); err != nil {
		t.Errorf("NewModel from the unhashed response = %v, want a model", err)
	}
	// A repeated call ID before the entry still refuses the path.
	dup, ids := repeatedCallIDs(t)
	if _, err := replay.NewModel(dup, replay.Strict(), replay.From(ids[len(ids)-1])); !errors.Is(err, replay.ErrCallIDRepeated) {
		t.Errorf("NewModel from after a repeated call ID = %v, want ErrCallIDRepeated", err)
	}
}

// Issue 45: Tools serve the outputs recorded after the entry, and none
// when the entry is not on the path or the session names no base.
func TestToolsFrom(t *testing.T) {
	s := recordedSession(t, [2]string{"call_1", `{"text":"one"}`}, [2]string{"call_2", `{"text":"two"}`})
	var firstOutput string
	for _, e := range s.Path(s.Leaf()) {
		if item, ok := e.(*agentsession.ItemEntry); ok {
			if _, ok := item.Item.(*openresponses.FunctionCallOutput); ok && firstOutput == "" {
				firstOutput = item.ID
			}
		}
	}
	tests := []struct {
		name   string
		opts   []replay.Option
		served map[string]bool
	}{
		{"the whole path", nil, map[string]bool{"call_1": true, "call_2": true}},
		{"from the first output", []replay.Option{replay.From(firstOutput)}, map[string]bool{"call_1": false, "call_2": true}},
		{"from an entry not on the path", []replay.Option{replay.From("sha256:nope")}, map[string]bool{"call_1": false, "call_2": false}},
		{"after the base of a session with none", []replay.Option{replay.AfterBase()}, map[string]bool{"call_1": false, "call_2": false}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ran := 0
			tools := replay.Tools(s, []agenttool.Tool{upperTool(&ran)}, append([]replay.Option{replay.Strict()}, tt.opts...)...)
			for id, want := range tt.served {
				_, err := tools[0].Execute(context.Background(), agenttool.Call{ID: id, Args: json.RawMessage(`{"text":"x"}`)})
				if got := err == nil; got != want {
					t.Errorf("%s served %v (%v), want %v", id, got, err, want)
				}
			}
			if ran != 0 {
				t.Errorf("the tool ran %d times under Strict", ran)
			}
		})
	}
}
