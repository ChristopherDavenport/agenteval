package replay_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agenteval/replay"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/compact"
	"github.com/ChristopherDavenport/agentturn/session"
	"github.com/ChristopherDavenport/openresponses"
	"github.com/ChristopherDavenport/openresponses/echo"
)

// A fold's model call is told from a turn's by the mark the fold puts
// on its context (agentturn RFC 0001, The request, step 2), not by the
// request's shape: a fold whose request carries instructions or tools
// is served as the fold, and a turn sent bare where the recording
// folded is a divergence (#70).
func TestFoldIsToldByItsMark(t *testing.T) {
	orig := loadFixture(t, "compaction")
	ctx := context.Background()
	// atFirstFold is a model that served the fixture's first step, a
	// response, so the next is its first fold.
	atFirstFold := func(t *testing.T) *replay.Model {
		t.Helper()
		model, err := replay.NewModel(orig)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := model.Create(ctx, openresponses.Request{Input: openresponses.Items{openresponses.UserText("one")}}); err != nil {
			t.Fatal(err)
		}
		return model
	}
	shaped := openresponses.Request{Instructions: "Summarise.", Tools: openresponses.Tools{&openresponses.FunctionTool{Name: "upper"}}, Input: openresponses.Items{openresponses.UserText("one")}}
	if _, err := atFirstFold(t).Create(compact.MarkFold(ctx), shaped); err != nil {
		t.Errorf("a marked fold with instructions and tools: %v", err)
	}
	bare := openresponses.Request{Input: openresponses.Items{openresponses.UserText("two")}}
	if _, err := atFirstFold(t).Create(ctx, bare); !errors.Is(err, replay.ErrDiverged) || !strings.Contains(err.Error(), "here and the request did not") {
		t.Errorf("an unmarked bare request at a fold: err = %v", err)
	}
}

// guardedEcho answers as the echo adapter does until it is told to
// refuse, then refuses as a moderation guard around the model does,
// after the provider billed the call and before anything of the
// answer opened.
type guardedEcho struct {
	refuse func(openresponses.Request) bool
}

func (g guardedEcho) CreateStream(ctx context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	if !g.refuse(req) {
		return (&echo.Adapter{}).CreateStream(ctx, req, sink)
	}
	em := openresponses.NewEmitter(sink, openresponses.NewResponse(req))
	em.Response().Usage = &openresponses.Usage{InputTokens: 11, OutputTokens: 3, TotalTokens: 14}
	if err := em.Start(); err != nil {
		return err
	}
	return fmt.Errorf("%w: the provider's filter refused the request", agentturn.ErrGuard)
}

// lastText is the text of the last user message of a request.
func lastText(req openresponses.Request) string {
	for i := len(req.Input) - 1; i >= 0; i-- {
		if m, ok := req.Input[i].(*openresponses.Message); ok && m.Role == openresponses.RoleUser {
			return m.Text()
		}
	}
	return ""
}

// blocked returns the agentturn:model_blocked records on the path.
func blocked(t *testing.T, s *agentsession.Session) []session.ModelBlocked {
	t.Helper()
	var out []session.ModelBlocked
	for _, e := range s.Path(s.Leaf()) {
		if c, ok := e.(*agentsession.CustomEntry); ok && c.NS == session.ModelBlockedNS {
			var b session.ModelBlocked
			if err := json.Unmarshal(c.Data, &b); err != nil {
				t.Fatal(err)
			}
			out = append(out, b)
		}
	}
	return out
}

// A call a guard refused is on the record as an agentturn:model_blocked
// entry. One the model's guard refused after the provider billed it is
// served again as that refusal, with its usage; one a BeforeModelCall
// guard refused never reached the model, and the replayed guard refuses
// it again, so the replay serves the call after it (#70).
func TestModelBlockedReplays(t *testing.T) {
	prompts := []string{"one", "refuse", "three"}
	for _, tc := range []struct {
		name  string
		setup func(cfg *agentturn.Config)
	}{
		{"by the model's guard", func(cfg *agentturn.Config) {
			cfg.Model = guardedEcho{refuse: func(req openresponses.Request) bool { return lastText(req) == "refuse" }}
		}},
		{"by BeforeModelCall", func(cfg *agentturn.Config) {
			cfg.BeforeModelCall = func(_ context.Context, req *openresponses.Request) error {
				if lastText(*req) == "refuse" {
					return fmt.Errorf("%w: policy", agentturn.ErrGuard)
				}
				return nil
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ran := 0
			cfg := fixtureConfig(&echo.Adapter{}, upperTool(&ran))
			tc.setup(&cfg)
			orig := record(t, cfg, prompts...)
			want := blocked(t, orig)
			if len(want) != 1 {
				t.Fatalf("%d model_blocked records", len(want))
			}
			model, err := replay.NewModel(orig, replay.Strict())
			if err != nil {
				t.Fatal(err)
			}
			cfg = fixtureConfig(model, replay.Tools(orig, []agenttool.Tool{upperTool(&ran)}, replay.Strict())...)
			cfg.BeforeModelCall = model.BeforeModelCall
			if tc.name == "by BeforeModelCall" {
				replayed := cfg.BeforeModelCall
				cfg.BeforeModelCall = func(ctx context.Context, req *openresponses.Request) error {
					if lastText(*req) == "refuse" {
						return fmt.Errorf("%w: policy", agentturn.ErrGuard)
					}
					return replayed(ctx, req)
				}
			}
			s := record(t, cfg, prompts...)
			got := blocked(t, s)
			if len(got) != 1 || got[0].RequestHash != want[0].RequestHash || !sameUsage(got[0].Usage, want[0].Usage) {
				t.Errorf("replayed model_blocked %+v, recorded %+v", got, want)
			}
			if model.Served() != model.Steps() {
				t.Errorf("served %d of %d steps", model.Served(), model.Steps())
			}
			if strings.Join(hashes(s), ",") != strings.Join(hashes(orig), ",") {
				t.Errorf("replayed hashes %v, recorded %v", hashes(s), hashes(orig))
			}
		})
	}
}

// record runs prompts through cfg in a fresh session, each a run of its
// own whatever the last one's end, and returns the session.
func record(t *testing.T, cfg agentturn.Config, prompts ...string) *agentsession.Session {
	t.Helper()
	rec, s, err := session.Start(context.Background(), agentsession.NewMemoryStore(), agentsession.Header{})
	if err != nil {
		t.Fatal(err)
	}
	a := agentturn.New(cfg)
	defer rec.Attach(a)()
	for _, p := range prompts {
		if _, err := a.Prompt(context.Background(), openresponses.UserText(p)); err != nil && !errors.Is(err, agentturn.ErrGuard) {
			t.Fatalf("prompt %q: %v", p, err)
		}
	}
	return s
}

// failedOutputs returns whether each function call output on the path
// is marked failed, in order.
func failedOutputs(s *agentsession.Session) []bool {
	var out []bool
	for _, e := range s.Path(s.Leaf()) {
		if it, ok := e.(*agentsession.ItemEntry); ok {
			if _, ok := it.Item.(*openresponses.FunctionCallOutput); ok {
				out = append(out, it.Failed)
			}
		}
	}
	return out
}

// An output marked failed, a tool's error, is served as that error, so
// the replayed record marks it failed too and the output the model sees
// is the same (#70).
func TestToolsServeAFailedOutputAsAFailure(t *testing.T) {
	failing := agenttool.New("upper", "Uppercase the text", func(context.Context, upperArgs) (string, error) {
		return "", errors.New("the disk is full")
	})
	orig := record(t, fixtureConfig(&echo.Adapter{}, failing), "hello")
	if f := failedOutputs(orig); len(f) != 1 || !f[0] {
		t.Fatalf("recorded outputs failed = %v", f)
	}
	model, err := replay.NewModel(orig, replay.Strict())
	if err != nil {
		t.Fatal(err)
	}
	ran := 0
	cfg := fixtureConfig(model, replay.Tools(orig, []agenttool.Tool{upperTool(&ran)}, replay.Strict())...)
	s := record(t, cfg, "hello")
	if f := failedOutputs(s); len(f) != 1 || !f[0] {
		t.Errorf("replayed outputs failed = %v, want the recorded failure", f)
	}
	if strings.Join(hashes(s), ",") != strings.Join(hashes(orig), ",") {
		t.Errorf("replayed hashes %v, recorded %v", hashes(s), hashes(orig))
	}
	if ran != 0 {
		t.Errorf("the real tool ran %d time(s)", ran)
	}
}

// A call carrying copied_from is another session's call, given to this
// one as context: it is not this session's, and a call of this session
// with the same name and arguments is not served its output (#70).
func TestToolsLeaveCopiedCallsOut(t *testing.T) {
	s := agentsession.New(agentsession.Header{})
	from := &agentsession.CopiedFrom{Session: "01995b2a-0000-7000-8000-00000000beef"}
	for _, e := range []agentsession.Entry{
		&agentsession.ItemEntry{Item: openresponses.UserText("x"), CopiedFrom: from},
		&agentsession.ItemEntry{Item: &openresponses.FunctionCall{CallID: "call_p", Name: "upper", Arguments: `{"text":"x"}`}, CopiedFrom: from},
		&agentsession.ItemEntry{Item: openresponses.NewFunctionCallOutput("call_p", "PARENT"), CopiedFrom: from},
	} {
		if _, err := s.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	callItem(t, s, "call_own", "y")
	outputItem(t, s, "call_own", "y")
	ran := 0
	tools := replay.Tools(s, []agenttool.Tool{upperTool(&ran)}, replay.Strict())
	if _, err := tools[0].Execute(context.Background(), agenttool.Call{ID: "call_new", Args: json.RawMessage(`{"text":"x"}`)}); !errors.Is(err, replay.ErrDiverged) {
		t.Errorf("a call matching the copied call: err = %v, want no recorded output", err)
	}
	if _, err := tools[0].Execute(context.Background(), agenttool.Call{ID: "call_p", Args: json.RawMessage(`{"text":"x"}`)}); !errors.Is(err, replay.ErrDiverged) {
		t.Errorf("a call with the copied call's ID: err = %v, want no recorded output", err)
	}
	res, err := tools[0].Execute(context.Background(), agenttool.Call{ID: "call_new", Args: json.RawMessage(`{"text":"y"}`)})
	if err != nil || res.Output.Text != "recorded:y" {
		t.Errorf("the session's own call: %+v, %v", res.Output, err)
	}
}

// pixel is a one-pixel PNG as a data URL.
var pixel = "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\nnot really a png"))

// A sidecar session's items name their media by sidecar: references,
// which no provider can fetch. A replay resolves each through the
// session's blob reader to the data URL it was before serving it, and
// hashes a request in the form the recorder did, each such medium named
// by its reference, so a strict replay of a sidecar session verifies;
// a task built from one is given the data URLs (#70).
func TestSidecarSessionReplays(t *testing.T) {
	ctx := context.Background()
	store := agentsession.NewMemoryStore()
	look := agenttool.New("upper", "Uppercase the text", func(context.Context, upperArgs) (agenttool.Result, error) {
		return agenttool.Result{Output: openresponses.FunctionCallOutputData{Parts: openresponses.Contents{&openresponses.InputText{Text: "a screenshot"}, &openresponses.InputImage{ImageURL: pixel}}}}, nil
	})
	rec, orig, err := session.Start(ctx, store, agentsession.Header{Media: agentsession.MediaSidecar})
	if err != nil {
		t.Fatal(err)
	}
	a := agentturn.New(fixtureConfig(&echo.Adapter{}, look))
	unsubscribe := rec.Attach(a)
	prompt := &openresponses.Message{Role: openresponses.RoleUser, Content: openresponses.Contents{&openresponses.InputText{Text: "hello"}, &openresponses.InputImage{ImageURL: pixel}}}
	if _, err := a.Prompt(ctx, prompt); err != nil {
		t.Fatal(err)
	}
	unsubscribe()
	line, _ := json.Marshal(orig.Entries())
	if !strings.Contains(string(line), "sidecar:sha256:") {
		t.Fatalf("the session names no blob: %s", entryTypes(orig))
	}
	var asked openresponses.Items
	for _, e := range orig.Path(orig.Leaf()) {
		if it, ok := e.(*agentsession.ItemEntry); ok {
			if m, ok := it.Item.(*openresponses.Message); ok && m.Role == openresponses.RoleUser {
				asked = append(asked, m)
			}
		}
	}
	prompts, err := replay.ResolveMedia(ctx, store, orig.ID(), asked)
	if err != nil {
		t.Fatal(err)
	}
	if data, _ := json.Marshal(prompts); strings.Contains(string(data), "sidecar:") || !strings.Contains(string(data), pixel) {
		t.Fatalf("resolved prompt %s", data)
	}
	model, err := replay.NewModel(orig, replay.Strict(), replay.WithBlobs(store))
	if err != nil {
		t.Fatal(err)
	}
	ran := 0
	cfg := fixtureConfig(model, replay.Tools(orig, []agenttool.Tool{upperTool(&ran)}, replay.Strict(), replay.WithBlobs(store))...)
	inline := agentsession.NewMemoryStore()
	rec2, s, err := session.Start(ctx, inline, agentsession.Header{})
	if err != nil {
		t.Fatal(err)
	}
	b := agentturn.New(cfg)
	defer rec2.Attach(b)()
	if _, err := b.Prompt(ctx, prompts[0]); err != nil {
		t.Fatal(err)
	}
	if data, _ := json.Marshal(s.Entries()); strings.Contains(string(data), "sidecar:") {
		t.Errorf("the replayed run was served sidecar: references: %s", data)
	}
	if model.Served() != model.Steps() || ran != 0 {
		t.Errorf("served %d of %d steps, the tool ran %d time(s)", model.Served(), model.Steps(), ran)
	}
	if _, err := replay.NewModel(orig); !errors.Is(err, session.ErrNoBlobReader) {
		t.Errorf("a sidecar session with no blob reader: err = %v", err)
	}
	if _, err := replay.ResolveMedia(ctx, nil, orig.ID(), asked); !errors.Is(err, session.ErrNoBlobReader) {
		t.Errorf("resolving with no blob reader: err = %v", err)
	}
}

// entryTypes renders a session's entry types on one line.
func entryTypes(s *agentsession.Session) string {
	var parts []string
	for _, e := range s.Entries() {
		parts = append(parts, e.EntryType())
	}
	return strings.Join(parts, " ")
}

// A BeforeModelCall guard's refusal records the request's hash though
// the request never reached the model. A replay whose configuration
// has no such guard sends that request: under Strict that is a
// divergence, not the recording's refusal served as the model's.
func TestUnsentBlockedCallIsNotServed(t *testing.T) {
	guard := func(_ context.Context, req *openresponses.Request) error {
		if lastText(*req) == "refuse" {
			return fmt.Errorf("%w: policy", agentturn.ErrGuard)
		}
		return nil
	}
	ran := 0
	cfg := fixtureConfig(&echo.Adapter{}, upperTool(&ran))
	cfg.BeforeModelCall = guard
	orig := record(t, cfg, "one", "refuse")
	if b := blocked(t, orig); len(b) != 1 || b[0].RequestHash == "" || b[0].Usage != nil {
		t.Fatalf("model_blocked records = %+v", b)
	}
	model, err := replay.NewModel(orig, replay.Strict())
	if err != nil {
		t.Fatal(err)
	}
	cfg = fixtureConfig(model, replay.Tools(orig, []agenttool.Tool{upperTool(&ran)}, replay.Strict())...)
	cfg.BeforeModelCall = model.BeforeModelCall
	a := agentturn.New(cfg)
	if _, err := a.Prompt(context.Background(), openresponses.UserText("one")); err != nil {
		t.Fatal(err)
	}
	end, err := a.Prompt(context.Background(), openresponses.UserText("refuse"))
	if err == nil && end != nil {
		err = end.Err
	}
	if !errors.Is(err, replay.ErrDiverged) || errors.Is(err, agentturn.ErrGuard) {
		t.Errorf("an unguarded replay of a guarded refusal: err = %v, want a divergence", err)
	}
}
