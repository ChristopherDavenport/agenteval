package agenteval_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agenteval"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/session"
	"github.com/ChristopherDavenport/openresponses"
)

// reasoner answers "ok" after a reasoning item, as a reasoning model
// does, and answers a fold's summary call (no tools, no instructions)
// with a message alone.
type reasoner struct{}

func (reasoner) CreateStream(_ context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	em := openresponses.NewEmitter(sink, openresponses.NewResponse(req))
	if len(req.Tools) > 0 || req.Instructions != "" {
		rw, err := em.Reasoning()
		if err != nil {
			return err
		}
		if err := rw.Summary("thinking"); err != nil {
			return err
		}
		rw.EncryptedContent("opaque-signature-of-model-1")
		if err := rw.Close(); err != nil {
			return err
		}
	}
	if err := em.Item(openresponses.AssistantText("ok")); err != nil {
		return err
	}
	return em.Complete()
}

// Issue 46: a fork of a base recorded on a reasoning model, run under
// another model, leaves the base's reasoning out of its request, which
// the format cannot describe; the recorder writes the response without
// a hash and an agentturn:unhashed entry saying why. The result's
// ErrUnreplayable quotes that entry rather than diagnosing a compacting
// configuration that never bound compact.WithOnFold, which this run
// does not have.
func TestUnreplayableQuotesTheUnhashedEntry(t *testing.T) {
	ctx := context.Background()
	store := newStableStore()
	base := echoConfig()
	base.Model = reasoner{}
	first, err := (&agenteval.Runner{Store: store, Config: func(agenteval.Task) agentturn.Config { return base }}).Run(ctx, &agenteval.Suite{Name: "base", Tasks: []agenteval.Task{{ID: "setup", Prompts: openresponses.Items{openresponses.UserText("look around")}}}})
	if err != nil {
		t.Fatal(err)
	}
	origin := first.Results[0]
	if origin.Err != nil {
		t.Fatalf("base: %v", origin.Err)
	}
	suite := &agenteval.Suite{Name: "fork", Tasks: []agenteval.Task{{ID: "next", Prompts: openresponses.Items{openresponses.UserText("go on")}}}}
	fork := func(t *testing.T, modelName string) (agenteval.Result, *session.Unhashed) {
		t.Helper()
		r := &agenteval.Runner{
			Store: store,
			ConfigWith: func(agenteval.Task, *session.Recorder) agentturn.Config {
				c := base
				c.ModelName = modelName
				return c
			},
			Header: func(agenteval.Task) agentsession.Header {
				return agentsession.Header{ParentSession: origin.SessionID, Base: origin.Target}
			},
		}
		rep, err := r.Run(ctx, suite)
		if err != nil {
			t.Fatal(err)
		}
		res := rep.Results[0]
		s, err := store.Open(ctx, res.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		var why *session.Unhashed
		for _, e := range s.Path(res.Target) {
			if c, ok := e.(*agentsession.CustomEntry); ok && c.NS == session.UnhashedNS {
				why = &session.Unhashed{}
				if err := json.Unmarshal(c.Data, why); err != nil {
					t.Fatal(err)
				}
			}
		}
		return res, why
	}

	// Under the base's model the reasoning goes back and every
	// response is hashed.
	same, why := fork(t, "echo/echo-1")
	if same.Err != nil || why != nil {
		t.Fatalf("fork under the base's model: err %v, unhashed entry %+v", same.Err, why)
	}

	other, why := fork(t, "other/model-2")
	if why == nil || why.Recorded == nil || why.Recorded.Type != openresponses.ItemTypeReasoning {
		t.Fatalf("fork under another model: no agentturn:unhashed entry naming the reasoning item left out: %+v", why)
	}
	if !errors.Is(other.Err, agenteval.ErrUnreplayable) {
		t.Fatalf("err = %v, want ErrUnreplayable", other.Err)
	}
	msg := other.Err.Error()
	if strings.Contains(msg, "WithOnFold") {
		t.Errorf("the error diagnoses a compacting configuration this run does not have:\n%s", msg)
	}
	for _, want := range []string{why.Reason, "reasoning", "agentsession#56"} {
		if !strings.Contains(msg, want) {
			t.Errorf("the error does not say %q:\n%s", want, msg)
		}
	}
}

// A fork is replayed after its base, so an unhashed response on the
// base's own path, which that replay never serves, does not make the
// fork unreplayable: here the base's BeforeModelCall added an item the
// record does not hold, and the fork, under a plain configuration,
// sends what the path rebuilds.
func TestForkOfAnUnhashedBaseIsReplayable(t *testing.T) {
	ctx := context.Background()
	store := newStableStore()
	edited := echoConfig()
	edited.BeforeModelCall = func(_ context.Context, req *openresponses.Request) error {
		// Put first, not last: the echo adapter calls its tool whenever
		// the last item is not a tool result, and a message appended
		// after one would keep it calling.
		req.Input = append(openresponses.Items{openresponses.UserText("remember the house style")}, req.Input...)
		return nil
	}
	first, err := (&agenteval.Runner{Store: store, Config: func(agenteval.Task) agentturn.Config { return edited }}).Run(ctx, &agenteval.Suite{Name: "base", Tasks: []agenteval.Task{{ID: "setup", Prompts: openresponses.Items{openresponses.UserText("look around")}}}})
	if err != nil {
		t.Fatal(err)
	}
	origin := first.Results[0]
	if !errors.Is(origin.Err, agenteval.ErrUnreplayable) {
		t.Fatalf("base: err %v, want ErrUnreplayable for the edited request", origin.Err)
	}
	r := &agenteval.Runner{
		Store:  store,
		Config: func(agenteval.Task) agentturn.Config { return echoConfig() },
		Header: func(agenteval.Task) agentsession.Header {
			return agentsession.Header{ParentSession: origin.SessionID, Base: origin.Target}
		},
	}
	rep, err := r.Run(ctx, &agenteval.Suite{Name: "fork", Tasks: []agenteval.Task{{ID: "next", Prompts: openresponses.Items{openresponses.UserText("go on")}}}})
	if err != nil {
		t.Fatal(err)
	}
	if res := rep.Results[0]; res.Err != nil {
		t.Errorf("fork of an unhashed base: %v", res.Err)
	}
}
