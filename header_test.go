package agenteval_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/ChristopherDavenport/agenteval"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/session"
	"github.com/ChristopherDavenport/openresponses"
)

// withHeader makes r start each session with the header members given.
func withHeader(r *agenteval.Runner, members map[string]string) *agenteval.Runner {
	r.Header = func(agenteval.Task) agentsession.Header {
		extra := map[string]json.RawMessage{}
		for k, v := range members {
			extra[k] = json.RawMessage(v)
		}
		return agentsession.Header{Extra: extra}
	}
	return r
}

// TestCompareHeaderSettings is #69: a setting that shapes the requests
// a harness builds and is not a request setting is a slug-prefixed
// header member (agentsession RFC 0001, Header, since v0.0.39), and a
// change to it during the session a custom entry whose ns is the slug.
// Two runs that differ only there, as the context study's check 7a
// does (a compaction budget of 1000 against 1000000, with no fold),
// compare as different configurations.
func TestCompareHeaderSettings(t *testing.T) {
	ctx := context.Background()
	suite := basicSuite(t)
	small := map[string]string{"dax.context": `{"compact_budget":1000}`}
	large := map[string]string{"dax.context": `{"compact_budget":1000000}`}

	c, err := agenteval.Compare(ctx, suite, withHeader(basicRunner(newStableStore()), small), withHeader(basicRunner(newStableStore()), large))
	if err != nil {
		t.Fatal(err)
	}
	if !c.Uniform || c.Config.Empty() {
		t.Fatalf("budgets 1000 and 1000000 compare as one configuration: uniform %v, config %+v", c.Uniform, c.Config)
	}
	ch, ok := c.Config.Header["dax.context"]
	if !ok || !sameValue(ch.A, `{"compact_budget":1000}`) || !sameValue(ch.B, `{"compact_budget":1000000}`) || len(c.Config.Header) != 1 || c.Config.BeyondSettings {
		t.Errorf("header diff = %+v, beyond settings %v", c.Config.Header, c.Config.BeyondSettings)
	}

	// A member only one side holds is nil on the other; the same value
	// written with other spacing or member order is no difference; a
	// member not named <slug>.<name> is not a harness setting; and a
	// member IgnoreHeader names, run identity, is left out.
	a := withHeader(basicRunner(newStableStore()), map[string]string{"dax.context": `{"a":1,"b":2}`, "dax.workspace": `{"root":"/a"}`, "legacy": `1`})
	b := withHeader(basicRunner(newStableStore()), map[string]string{"dax.context": `{ "b": 2, "a": 1 }`, "dax.workspace": `{"root":"/b"}`, "acme.cap": `4096`, "legacy": `2`})
	c, err = agenteval.Compare(ctx, suite, a, b, agenteval.IgnoreHeader("dax.workspace"))
	if err != nil {
		t.Fatal(err)
	}
	if ch, ok := c.Config.Header["acme.cap"]; !ok || ch.A != nil || ch.B != float64(4096) || len(c.Config.Header) != 1 {
		t.Errorf("header diff = %+v", c.Config.Header)
	}
	c, err = agenteval.Compare(ctx, suite, a, b)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Config.Header["dax.workspace"]; !ok || len(c.Config.Header) != 2 {
		t.Errorf("without IgnoreHeader, header diff = %+v", c.Config.Header)
	}

	// The same header, and B's run changes the budget mid-session.
	a = withHeader(basicRunner(newStableStore()), small)
	b = withHeader(basicRunner(newStableStore()), small)
	b.ConfigWith = func(task agenteval.Task, rec *session.Recorder) agentturn.Config {
		cfg := basicConfig(task)
		var once sync.Once
		cfg.BeforeTurn = func(ctx context.Context, _ agentturn.TurnStartInfo) (items openresponses.Items, err error) {
			once.Do(func() {
				_, err = rec.Annotate(ctx, "dax", map[string]any{"dax.context": map[string]any{"compact_budget": 500}})
			})
			return nil, err
		}
		return cfg
	}
	c, err = agenteval.Compare(ctx, suite, a, b)
	if err != nil {
		t.Fatal(err)
	}
	if c.Config.Empty() {
		t.Fatalf("a budget changed mid-session compares as no change: %+v", c.Config)
	}
	ch, ok = c.Config.HeaderChanged["dax.context"]
	if !ok || ch.A != nil || !sameValue(ch.B, `[{"compact_budget":500}]`) || len(c.Config.Header) != 0 {
		t.Errorf("header changed = %+v, header %+v", c.Config.HeaderChanged, c.Config.Header)
	}

	// Read back from the stores, a comparison of the reports says the
	// same.
	stored, err := agenteval.CompareReports(ctx, c.A, c.B, agenteval.OneStore(a.Store), agenteval.OneStore(b.Store))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := stored.Config.HeaderChanged["dax.context"]; !ok {
		t.Errorf("stored header changed = %+v", stored.Config.HeaderChanged)
	}
}

// sameValue says whether v, decoded JSON, is the JSON value want.
func sameValue(v any, want string) bool {
	got, err := json.Marshal(v)
	if err != nil {
		return false
	}
	var w any
	if err := json.Unmarshal([]byte(want), &w); err != nil {
		return false
	}
	canon, _ := json.Marshal(w)
	return string(got) == string(canon)
}
