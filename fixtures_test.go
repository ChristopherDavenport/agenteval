package agenteval_test

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/compact"
	"github.com/ChristopherDavenport/agentturn/session"
	"github.com/ChristopherDavenport/openresponses"
	"github.com/ChristopherDavenport/openresponses/echo"
)

// fixtures are the sessions under testdata/sessions, recorded from the
// echo adapter through a stable store. Each records a session from
// scratch and returns it.
var fixtures = map[string]func(t *testing.T) *agentsession.Session{
	// basic: one prompt, the echo adapter calls upper, then answers.
	"basic": func(t *testing.T) *agentsession.Session {
		return record(t, echoConfig(), nil, "hello world")
	},
	// multi: two prompts, two runs, no tools.
	"multi": func(t *testing.T) *agentsession.Session {
		cfg := echoConfig()
		cfg.Tools = nil
		return record(t, cfg, nil, "first", "second")
	},
	// compaction: three prompts under a local fold with a budget so
	// small that every call past the first folds; the folds are model
	// calls the session records only as compaction entries.
	"compaction": func(t *testing.T) *agentsession.Session {
		cfg := echoConfig()
		return record(t, cfg, func(cfg *agentturn.Config, rec *session.Recorder) {
			c := compact.NewLocal(cfg.Model, compact.WithBudget(1), compact.WithKeepLast(2), compact.WithOnFold(rec.Fold))
			cfg.Transform = c.Transform
		}, "one", "two", "three")
	},
	// endpoint: as compaction, but folding through the adapter's own
	// compaction endpoint, so each fold's summary is a compaction item.
	"endpoint": func(t *testing.T) *agentsession.Session {
		cfg := echoConfig()
		return record(t, cfg, func(cfg *agentturn.Config, rec *session.Recorder) {
			c := compact.New(cfg.Model.(*echo.Adapter), compact.WithBudget(1), compact.WithKeepLast(2), compact.WithOnFold(rec.Fold))
			cfg.Transform = c.Transform
		}, "one", "two", "three")
	},
	// layers: a configuration whose instructions are rebuilt before
	// every call from state the run itself writes, as a memory block
	// or a skill set is rebuilt. The session carries a config delta
	// mid-run, and a replay that serves the model from the record and
	// not the settings sends what those layers say today.
	"layers": func(t *testing.T) *agentsession.Session {
		notes := 0
		cfg := echoConfig()
		cfg.BeforeModelCall = func(_ context.Context, req *openresponses.Request) error {
			req.Instructions = layerInstructions(notes)
			return nil
		}
		cfg.AfterToolCall = func(context.Context, agentturn.ToolResultInfo) (*agentturn.ToolOverride, error) {
			notes++
			return nil, nil
		}
		return record(t, cfg, nil, "hello world")
	},
	// roots: two roots in one session, so the session has two leaves
	// and a replay must name the one it wants.
	"roots": func(t *testing.T) *agentsession.Session {
		store := newStableStore()
		rec, s, err := session.Start(context.Background(), store, agentsession.Header{Harness: &agentsession.Harness{Name: "fixture", Version: "1"}})
		if err != nil {
			t.Fatal(err)
		}
		cfg := echoConfig()
		cfg.Tools = nil
		a := agentturn.New(cfg)
		defer rec.Attach(a)()
		if _, err := a.Prompt(context.Background(), openresponses.UserText("hello")); err != nil {
			t.Fatal(err)
		}
		s.ResetLeaf()
		b := agentturn.New(cfg)
		rec2 := session.New(store, s.ID())
		defer rec2.Attach(b)()
		if _, err := b.Prompt(context.Background(), openresponses.UserText("goodbye")); err != nil {
			t.Fatal(err)
		}
		return s
	},
}

// layerInstructions is what the layers fixture's product renders from
// the state it has written so far. The replay package's test builds
// the same string, because a replay of that session is the product
// running again against state that has moved on.
func layerInstructions(notes int) string {
	if notes == 0 {
		return "Be brief."
	}
	return fmt.Sprintf("Be brief. Notes: %d", notes)
}

// record runs prompts through cfg with a recorder attached and returns
// the session. setup, when given, adjusts the configuration once the
// recorder exists, for a transform that reports folds to it.
func record(t *testing.T, cfg agentturn.Config, setup func(*agentturn.Config, *session.Recorder), prompts ...string) *agentsession.Session {
	t.Helper()
	store := newStableStore()
	rec, s, err := session.Start(context.Background(), store, agentsession.Header{Harness: &agentsession.Harness{Name: "fixture", Version: "1"}})
	if err != nil {
		t.Fatal(err)
	}
	if setup != nil {
		setup(&cfg, rec)
	}
	a := agentturn.New(cfg)
	defer rec.Attach(a)()
	for _, p := range prompts {
		if _, err := a.Prompt(context.Background(), openresponses.UserText(p)); err != nil {
			t.Fatalf("prompt %q: %v", p, err)
		}
	}
	return s
}

// TestFixtures rewrites the fixtures with -update, and otherwise checks
// that each one on disk is a session every response of which rebuilds
// to its recorded hash, which is what a strict replay relies on.
func TestFixtures(t *testing.T) {
	for name, build := range fixtures {
		t.Run(name, func(t *testing.T) {
			if *update {
				writeSession(t, build(t), filepath.Join("testdata", "sessions", name+".jsonl"))
				return
			}
			s := loadFixture(t, name)
			if n := verifyAll(t, s); n == 0 {
				t.Error("fixture has no responses")
			}
		})
	}
}

func TestFixtureShapes(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{"basic", "run config item:user item:function_call* response dispatch item:function_call_output item:assistant* response run"},
		{"multi", "run config item:user item:assistant* response run run item:user item:assistant* response run"},
		{"roots", "run config item:user item:assistant* response run run config item:user item:assistant* response run"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := entryTypes(loadFixture(t, tt.name)); got != tt.want {
				t.Errorf("entries = %q\nwant      %q", got, tt.want)
			}
		})
	}
	for _, name := range []string{"compaction", "endpoint"} {
		s := loadFixture(t, name)
		folds := 0
		for _, e := range s.Entries() {
			if c, ok := e.(*agentsession.CompactionEntry); ok {
				folds++
				_, isComp := c.Summary.(*openresponses.Compaction)
				if isComp != (name == "endpoint") {
					t.Errorf("%s: fold %s summary is %T", name, c.ID, c.Summary)
				}
			}
		}
		if folds == 0 {
			t.Errorf("%s fixture has no compaction entries: %s", name, entryTypes(s))
		}
	}
}
