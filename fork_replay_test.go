package agenteval_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agenteval"
	"github.com/ChristopherDavenport/agenteval/replay"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/session"
	"github.com/ChristopherDavenport/openresponses"
)

// Issue 45: a task forked through Runner.Header replays through the
// runner with the same Header only under replay.AfterBase, since the
// runner seeds the fork's agent at the base and the agent never sends
// the base's requests; without it the replay model serves the base's
// first response against the fork's first request and diverges at
// step 1. Replayed whole, from an unseeded agent that sends the base's
// prompts too, the same session serves every step without the option.
func TestForkReplaysThroughTheRunnerAfterItsBase(t *testing.T) {
	ctx := context.Background()
	store := newStableStore()
	cfg := echoConfig()
	cfg.Tools = nil
	config := func(agenteval.Task) agentturn.Config { return cfg }
	first, err := (&agenteval.Runner{Store: store, Config: config}).Run(ctx, &agenteval.Suite{Name: "base", Tasks: []agenteval.Task{{ID: "setup", Prompts: openresponses.Items{openresponses.UserText("look around")}}}})
	if err != nil {
		t.Fatal(err)
	}
	origin := first.Results[0]
	if origin.Err != nil {
		t.Fatalf("base: %v", origin.Err)
	}
	header := func(agenteval.Task) agentsession.Header {
		return agentsession.Header{ParentSession: origin.SessionID, Base: origin.Target}
	}
	suite := &agenteval.Suite{Name: "fork", Tasks: []agenteval.Task{{ID: "next", Prompts: openresponses.Items{openresponses.UserText("go on")}}}}
	forked, err := (&agenteval.Runner{Store: store, Config: config, Header: header}).Run(ctx, suite)
	if err != nil {
		t.Fatal(err)
	}
	res := forked.Results[0]
	if res.Err != nil {
		t.Fatalf("fork: %v", res.Err)
	}
	s, err := store.Open(ctx, res.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := replay.Unverifiable(s, res.Target); err != nil {
		t.Fatalf("Unverifiable = %v: every response of the fork should be hashed", err)
	}
	if s.Header().Base != origin.Target {
		t.Fatalf("the fork's base is %q, want %s", s.Header().Base, origin.Target)
	}

	// through replays the fork through a runner with the same Header,
	// as a suite of continuations is replayed nightly.
	through := func(t *testing.T, opts ...replay.Option) (*replay.Model, error) {
		t.Helper()
		model, err := replay.NewModel(s, append([]replay.Option{replay.Strict(), replay.WithLeaf(res.Target)}, opts...)...)
		if err != nil {
			t.Fatal(err)
		}
		r := &agenteval.Runner{
			Store: store,
			ConfigWith: func(agenteval.Task, *session.Recorder) agentturn.Config {
				c := cfg
				c.Model = model
				c.BeforeModelCall = model.BeforeModelCall
				c.Tools = replay.Tools(s, nil, append([]replay.Option{replay.Strict(), replay.WithLeaf(res.Target)}, opts...)...)
				return c
			},
			Header: header,
		}
		rep, err := r.Run(ctx, suite)
		if err != nil {
			t.Fatal(err)
		}
		return model, rep.Results[0].Err
	}
	tests := []struct {
		name string
		opts []replay.Option
		// served is how many steps the model serves, and diverges
		// whether the run ends on ErrDiverged at step 1.
		served   int
		diverges bool
	}{
		{"without the option, against the base's first response", nil, 1, true},
		{"after the base", []replay.Option{replay.AfterBase()}, 1, false},
		{"from the base", []replay.Option{replay.From(origin.Target)}, 1, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model, err := through(t, tt.opts...)
			if tt.diverges {
				if !errors.Is(err, replay.ErrDiverged) || !strings.Contains(err.Error(), "(step 1)") {
					t.Fatalf("err = %v, want ErrDiverged at step 1", err)
				}
			} else if err != nil {
				t.Fatalf("err = %v", err)
			}
			if model.Served() != tt.served {
				t.Errorf("served %d of %d steps, want %d", model.Served(), model.Steps(), tt.served)
			}
			if !tt.diverges && model.Served() != model.Steps() {
				t.Errorf("served %d of %d steps", model.Served(), model.Steps())
			}
		})
	}

	// The whole path, from an unseeded agent sending the base's prompt
	// and the task's.
	whole, err := replay.NewModel(s, replay.Strict(), replay.WithLeaf(res.Target))
	if err != nil {
		t.Fatal(err)
	}
	c := cfg
	c.Model = whole
	c.BeforeModelCall = whole.BeforeModelCall
	a := agentturn.New(c)
	for _, p := range []string{"look around", "go on"} {
		if _, err := a.Prompt(ctx, openresponses.UserText(p)); err != nil {
			t.Fatalf("whole: prompt %q: %v", p, err)
		}
	}
	if whole.Served() != whole.Steps() || whole.Steps() != 2 {
		t.Errorf("whole: served %d of %d steps, want 2 of 2", whole.Served(), whole.Steps())
	}
}
