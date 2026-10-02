package agenteval_test

import (
	"context"
	"encoding/json"
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

// A fork of a base recorded on a reasoning model, run under another
// model, no longer leaves its responses unhashed: agentsession format
// 0.11 lets the recorder write an omit of the reasoning at the config
// entry that switches the model and hash later requests against the
// context rebuilt under that rule. A strict replay of the fork serves
// every response, without AllowUnhashed.
func TestModelSwitchReplaysStrictWithoutAllowUnhashed(t *testing.T) {
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
	baseSession, err := store.Open(ctx, origin.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	reasoning := false
	for _, e := range baseSession.Path(origin.Target) {
		if it, ok := e.(*agentsession.ItemEntry); ok && it.Item.ItemType() == openresponses.ItemTypeReasoning {
			reasoning = true
		}
	}
	if !reasoning {
		t.Fatal("the base path holds no reasoning item, so the fork would not exercise the switch")
	}
	suite := &agenteval.Suite{Name: "fork", Tasks: []agenteval.Task{{ID: "next", Prompts: openresponses.Items{openresponses.UserText("go on")}}}}
	r := &agenteval.Runner{
		Store: store,
		ConfigWith: func(agenteval.Task, *session.Recorder) agentturn.Config {
			c := base
			c.ModelName = "other/model-2"
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
	if res.Err != nil {
		t.Fatalf("fork under another model: %v", res.Err)
	}
	s, err := store.Open(ctx, res.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	// Every response after the switch carries a hash, not just one.
	omitted, after := false, 0
	for _, e := range s.Path(res.Target) {
		switch e := e.(type) {
		case *agentsession.CustomEntry:
			if e.NS == session.UnhashedNS {
				t.Errorf("an agentturn:unhashed entry was recorded: %s", e.Data)
			}
		case *agentsession.ResponseEntry:
			if omitted {
				after++
				if e.RequestHash == "" {
					t.Errorf("response %s after the switch has no request_hash", e.ID)
				}
			}
		case *agentsession.ConfigEntry:
			if e.Omit != nil && e.Omit.Reasoning == agentsession.OmitOtherModels {
				omitted = true
			}
		}
	}
	if !omitted || after == 0 {
		t.Errorf("omit entry %v, responses after it %d; want an omit and a response after it", omitted, after)
	}
	if err := replay.Unverifiable(s, res.Target); err != nil {
		t.Errorf("Unverifiable: %v", err)
	}
	if _, err := replay.NewModel(s, replay.Strict(), replay.WithLeaf(res.Target)); err != nil {
		t.Errorf("strict NewModel without AllowUnhashed: %v", err)
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
	// Issue 46: the error quotes the agentturn:unhashed entry's reason.
	bs, err := store.Open(ctx, origin.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	var why *session.Unhashed
	for _, e := range bs.Path(origin.Target) {
		if c, ok := e.(*agentsession.CustomEntry); ok && c.NS == session.UnhashedNS {
			why = &session.Unhashed{}
			if err := json.Unmarshal(c.Data, why); err != nil {
				t.Fatal(err)
			}
		}
	}
	if why == nil || why.Reason == "" {
		t.Fatalf("base: no agentturn:unhashed entry with a reason: %+v", why)
	}
	if msg := origin.Err.Error(); !strings.Contains(msg, why.Reason) {
		t.Errorf("base: the error does not quote the unhashed entry's reason %q:\n%s", why.Reason, msg)
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
