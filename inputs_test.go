package agenteval_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agenteval"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/session"
	"github.com/ChristopherDavenport/agentturn/tools/agent"
	"github.com/ChristopherDavenport/openresponses"
	"github.com/ChristopherDavenport/openresponses/echo"
)

// promptTexts is a task's prompts as one line.
func promptTexts(task agenteval.Task) string {
	var out []string
	for _, p := range task.Prompts {
		if m, ok := p.(*openresponses.Message); ok {
			out = append(out, m.Text())
		}
	}
	return strings.Join(out, " | ")
}

// TestFromSessionTakesOnlyInputs is #57: the prompts of a task built
// from a session are the inputs that started its runs (RFC 0001, run).
// A resume's note, a steer drained mid-run and an item copied from
// another session's conversation are user messages too, and none of
// them started a turn; each is left out and named in Meta, rather than
// sent again as a turn of its own.
func TestFromSessionTakesOnlyInputs(t *testing.T) {
	ctx := context.Background()

	t.Run("noted resume", func(t *testing.T) {
		store := newStableStore()
		r := &agenteval.Runner{
			Store:  store,
			Config: func(agenteval.Task) agentturn.Config { return deferring(nil) },
			Answer: declineAfter(1, func(_ context.Context, end *agentturn.RunEnd) ([]agentturn.Answer, error) {
				return []agentturn.Answer{agentturn.Approve(end.Pending[0].Call.CallID).WithNote("go ahead")}, nil
			}),
		}
		report, err := r.Run(ctx, &agenteval.Suite{Name: "s", Tasks: []agenteval.Task{{ID: "one", Instruction: "hello"}}})
		if err != nil {
			t.Fatal(err)
		}
		res := report.Results[0]
		if res.Resumes != 1 {
			t.Fatalf("resumes = %d, err = %v", res.Resumes, res.Err)
		}
		s, err := store.Open(ctx, res.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		task, err := agenteval.FromSessionAt(s, res.Target)
		if err != nil {
			t.Fatal(err)
		}
		if got := promptTexts(task); got != "hello" {
			t.Errorf("prompts = %q, want the input alone, not the resume's note", got)
		}
		if lo := task.Meta[agenteval.LeftOutMeta]; !strings.Contains(lo, agentsession.NotInputResume+"=") {
			t.Errorf("meta %s = %q, want the note named", agenteval.LeftOutMeta, lo)
		}
	})

	t.Run("steer", func(t *testing.T) {
		store := newStableStore()
		rec, s, err := session.Start(ctx, store, agentsession.Header{})
		if err != nil {
			t.Fatal(err)
		}
		var a *agentturn.Agent
		steered := false
		steer := agenttool.New("upper", "Uppercase the text", func(_ context.Context, in upperArgs) (string, error) {
			if !steered {
				steered = true
				a.Steer(openresponses.UserText("and shout"))
			}
			return strings.ToUpper(in.Text), nil
		})
		cfg := echoConfig()
		cfg.Tools = []agenttool.Tool{steer}
		a = agentturn.New(cfg)
		defer rec.Attach(a)()
		if _, err := a.Prompt(ctx, openresponses.UserText("hello")); err != nil {
			t.Fatal(err)
		}
		task, err := agenteval.FromSession(s)
		if err != nil {
			t.Fatal(err)
		}
		if got := promptTexts(task); got != "hello" {
			t.Errorf("prompts = %q, want the input alone, not the steer (%s)", got, entryTypes(s))
		}
		if lo := task.Meta[agenteval.LeftOutMeta]; !strings.Contains(lo, agentsession.NotInputSteer+"=") {
			t.Errorf("meta %s = %q, want the steer named", agenteval.LeftOutMeta, lo)
		}
	})

	t.Run("copied", func(t *testing.T) {
		store := newStableStore()
		rec, s, err := session.Start(ctx, store, agentsession.Header{})
		if err != nil {
			t.Fatal(err)
		}
		child := agentturn.Config{Name: "specialist", Description: "answers", Model: &echo.Adapter{}}
		cfg := echoConfig()
		// The child is prompted with the parent's conversation so far
		// and then its own input: the echo adapter fills input with the
		// prompt's text.
		type taskArgs struct {
			Input string `json:"input"`
		}
		cfg.Tools = []agenttool.Tool{agent.New(child,
			agent.WithCallConfig(func(_ context.Context, in taskArgs, parent agentturn.Transcript, cfg agentturn.Config) (agentturn.Config, openresponses.Items, error) {
				return cfg, append(append(openresponses.Items(nil), parent...), openresponses.UserText(in.Input)), nil
			}),
			agent.WithObserver(rec.Observe), agent.WithRunContext(rec.ChildContext))}
		a := agentturn.New(cfg)
		defer rec.Attach(a)()
		if _, err := a.Prompt(ctx, openresponses.UserText("hello")); err != nil {
			t.Fatal(err)
		}
		var childID string
		for _, e := range s.Entries() {
			if l, ok := e.(*agentsession.LinkEntry); ok && l.Rel == agentsession.RelSubsession {
				childID = l.Session
			}
		}
		cs, err := store.Open(ctx, childID)
		if err != nil {
			t.Fatalf("child %q: %v", childID, err)
		}
		copied := 0
		for _, e := range cs.Entries() {
			if it, ok := e.(*agentsession.ItemEntry); ok && it.CopiedFrom != nil {
				copied++
			}
		}
		if copied == 0 {
			t.Fatalf("the child holds no copied item: %s", entryTypes(cs))
		}
		task, err := agenteval.FromSession(cs)
		if err != nil {
			t.Fatal(err)
		}
		if got := promptTexts(task); got != "hello" {
			t.Errorf("prompts = %q, want the child's own input alone, not the parent's message it was given (%s)", got, entryTypes(cs))
		}
		if lo := task.Meta[agenteval.LeftOutMeta]; !strings.Contains(lo, agentsession.NotInputCopied+"=") {
			t.Errorf("meta %s = %q, want the copied items named (prompts %q)", agenteval.LeftOutMeta, lo, promptTexts(task))
		}
	})
}

// TestFromSessionResolvesSidecarMedia is #70's task half: a sidecar
// session's prompts name their media by sidecar: references, which no
// provider can fetch. FromSession refuses to hand one out, and
// FromSessionMedia resolves each through the session's blob reader to
// the data URL it was.
func TestFromSessionResolvesSidecarMedia(t *testing.T) {
	ctx := context.Background()
	store := agentsession.NewMemoryStore()
	rec, s, err := session.Start(ctx, store, agentsession.Header{Media: agentsession.MediaSidecar})
	if err != nil {
		t.Fatal(err)
	}
	cfg := echoConfig()
	cfg.Tools = nil
	a := agentturn.New(cfg)
	defer rec.Attach(a)()
	pixel := "data:image/png;base64,iVBORw0KGgo="
	prompt := &openresponses.Message{Role: openresponses.RoleUser, Content: openresponses.Contents{&openresponses.InputText{Text: "look"}, &openresponses.InputImage{ImageURL: pixel}}}
	if _, err := a.Prompt(ctx, prompt); err != nil {
		t.Fatal(err)
	}
	if _, err := agenteval.FromSession(s); !errors.Is(err, session.ErrNoBlobReader) {
		t.Errorf("FromSession of a sidecar session: err = %v, want session.ErrNoBlobReader", err)
	}
	task, err := agenteval.FromSessionMedia(ctx, s, s.Leaf(), store)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(task.Prompts)
	if strings.Contains(string(data), "sidecar:") || !strings.Contains(string(data), pixel) {
		t.Errorf("prompts = %s, want the data URL", data)
	}
}

// One run that took two input messages, a prompt of two items, is one
// run of the task built from it, not two (review of #57).
func TestFromSessionKeepsARunsInputsTogether(t *testing.T) {
	ctx := context.Background()
	store := newStableStore()
	rec, s, err := session.Start(ctx, store, agentsession.Header{})
	if err != nil {
		t.Fatal(err)
	}
	cfg := echoConfig()
	cfg.Tools = nil
	a := agentturn.New(cfg)
	unsubscribe := rec.Attach(a)
	if _, err := a.Prompt(ctx, openresponses.UserText("first"), openresponses.UserText("second")); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Prompt(ctx, openresponses.UserText("third")); err != nil {
		t.Fatal(err)
	}
	unsubscribe()
	task, err := agenteval.FromSession(s)
	if err != nil {
		t.Fatal(err)
	}
	runs := task.Runs()
	if len(runs) != 2 || len(runs[0]) != 2 || len(runs[1]) != 1 {
		t.Fatalf("runs = %v", runs)
	}
	r := &agenteval.Runner{Store: store, Config: func(agenteval.Task) agentturn.Config { return cfg }}
	report, err := r.Run(ctx, &agenteval.Suite{Name: "s", Tasks: []agenteval.Task{task}})
	if err != nil {
		t.Fatal(err)
	}
	if res := report.Results[0]; res.Err != nil || res.Runs != 2 {
		t.Errorf("runs = %d, err = %v, want the recording's two", res.Runs, res.Err)
	}
}
