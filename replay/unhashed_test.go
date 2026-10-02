package replay_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agenteval/replay"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentturn/session"
	"github.com/ChristopherDavenport/openresponses"
)

// unhashedEntry is the agentturn:unhashed entry the recorder writes
// before a response it leaves without a hash.
func unhashedEntry(t *testing.T, why session.Unhashed) *agentsession.CustomEntry {
	t.Helper()
	data, err := json.Marshal(why)
	if err != nil {
		t.Fatal(err)
	}
	return &agentsession.CustomEntry{NS: session.UnhashedNS, Data: data}
}

// Issue 46: ErrUnverifiable for a path with unhashed responses quotes
// the agentturn:unhashed entry that explains the first of them, the
// last such entry before it, and adds why a reasoning item cannot be
// described; an entry a hashed response follows explains nothing.
func TestUnverifiableQuotesTheUnhashedEntry(t *testing.T) {
	hashed := "sha256:" + strings.Repeat("0", 64)
	response := func(id, hash string) agentsession.Entry {
		return &agentsession.ResponseEntry{ResponseID: id, Model: "m", Status: openresponses.ResponseStatusCompleted, RequestHash: hash}
	}
	reasoning := session.Unhashed{
		Reason: "the request's input differs from the input the recorded path rebuilds", Index: 1,
		Sent: &session.UnhashedItem{Type: "message"}, Recorded: &session.UnhashedItem{Type: "reasoning", ID: "rs_1"},
	}
	unnamed := session.Unhashed{Reason: "the response named no ID, so the items it produced read as its input", Index: 2, Sent: &session.UnhashedItem{Type: "message"}}
	tests := []struct {
		name     string
		entries  func(t *testing.T) []agentsession.Entry
		want     []string
		unwanted []string
	}{
		{"a reasoning item left out", func(t *testing.T) []agentsession.Entry {
			return []agentsession.Entry{unhashedEntry(t, reasoning), response("resp_1", "")}
		}, []string{"1 of 1 responses", reasoning.Reason, "index 1: sent message, recorded reasoning", "another model's reasoning", "agentsession#56"}, nil},
		{"a response that named no ID", func(t *testing.T) []agentsession.Entry {
			return []agentsession.Entry{unhashedEntry(t, unnamed), response("", "")}
		}, []string{unnamed.Reason, "index 2: sent message, recorded nothing"}, []string{"agentsession#56"}},
		{"the last entry before the first unhashed response", func(t *testing.T) []agentsession.Entry {
			return []agentsession.Entry{unhashedEntry(t, unnamed), unhashedEntry(t, reasoning), response("resp_1", ""), response("resp_2", "")}
		}, []string{"2 of 2 responses", reasoning.Reason}, []string{unnamed.Reason}},
		{"an entry a hashed response follows explains nothing", func(t *testing.T) []agentsession.Entry {
			return []agentsession.Entry{unhashedEntry(t, reasoning), response("resp_1", hashed), response("resp_2", "")}
		}, []string{"1 of 2 responses"}, []string{reasoning.Reason}},
		{"no entry", func(t *testing.T) []agentsession.Entry {
			return []agentsession.Entry{response("resp_1", "")}
		}, []string{"1 of 1 responses"}, []string{"says why"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := agentsession.New(agentsession.Header{})
			for _, e := range append([]agentsession.Entry{agentsession.NewItemEntry(openresponses.UserText("hi"))}, tt.entries(t)...) {
				if _, err := s.Append(e); err != nil {
					t.Fatal(err)
				}
			}
			err := replay.Unverifiable(s, "")
			if !errors.Is(err, replay.ErrUnverifiable) {
				t.Fatalf("Unverifiable = %v, want ErrUnverifiable", err)
			}
			for _, w := range tt.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("the error does not say %q:\n%v", w, err)
				}
			}
			for _, u := range tt.unwanted {
				if strings.Contains(err.Error(), u) {
					t.Errorf("the error says %q, which does not apply:\n%v", u, err)
				}
			}
		})
	}
}
