package replay_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agenteval/replay"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/compact"
	"github.com/ChristopherDavenport/agentturn/session"
	"github.com/ChristopherDavenport/openresponses"
)

type upperArgs struct {
	Text string `json:"text"`
}

// upper matches the fixtures' tool by name and schema; its body counts
// how often the real tool ran, which a replay must never do.
func upperTool(ran *int) agenttool.Tool {
	return agenttool.New("upper", "Uppercase the text", func(_ context.Context, a upperArgs) (string, error) {
		*ran++
		return strings.ToUpper(a.Text), nil
	})
}

// fixtureConfig is the configuration the fixtures were recorded under,
// with model behind it.
func fixtureConfig(model agentturn.Model, tools ...agenttool.Tool) agentturn.Config {
	return agentturn.Config{Model: model, ModelName: "echo/echo-1", Instructions: "Be brief.", Tools: tools}
}

func loadFixture(t *testing.T, name string) *agentsession.Session {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "testdata", "sessions", name+".jsonl"))
	if err != nil {
		t.Fatalf("%v (run go test . -update at the root to create it)", err)
	}
	defer f.Close()
	s, err := agentsession.Read(f)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// hashes returns the request hashes of the response entries on the
// path to the session's current leaf, in order.
func hashes(s *agentsession.Session) []string {
	var out []string
	for _, e := range s.Path(s.Leaf()) {
		if r, ok := e.(*agentsession.ResponseEntry); ok {
			out = append(out, r.RequestHash)
		}
	}
	return out
}

// rerun runs prompts through cfg with a fresh recorder and returns the
// new session and the last run's end.
func rerun(t *testing.T, cfg agentturn.Config, prompts ...string) (*agentsession.Session, *agentturn.RunEnd, error) {
	t.Helper()
	return rerunWith(t, cfg, nil, prompts...)
}

// rerunWith is rerun with a setup that adjusts the configuration once
// the recorder exists, for a transform that reports folds to it; the
// recorder hashes a request only when the path it wrote rebuilds the
// input, which after a fold needs the fold reported.
func rerunWith(t *testing.T, cfg agentturn.Config, setup func(*agentturn.Config, *session.Recorder), prompts ...string) (*agentsession.Session, *agentturn.RunEnd, error) {
	t.Helper()
	store := agentsession.NewMemoryStore()
	rec, s, err := session.Start(context.Background(), store, agentsession.Header{})
	if err != nil {
		t.Fatal(err)
	}
	if setup != nil {
		setup(&cfg, rec)
	}
	a := agentturn.New(cfg)
	defer rec.Attach(a)()
	var end *agentturn.RunEnd
	for _, p := range prompts {
		end, err = a.Prompt(context.Background(), openresponses.UserText(p))
		if err != nil {
			return s, end, err
		}
	}
	return s, end, nil
}

func TestStrictReplayMatchesRecording(t *testing.T) {
	tests := []struct {
		fixture string
		tools   bool
		prompts []string
	}{
		{"basic", true, []string{"hello world"}},
		{"multi", false, []string{"first", "second"}},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			orig := loadFixture(t, tt.fixture)
			var seen []replay.Served
			model, err := replay.NewModel(orig, replay.Strict(), replay.WithObserver(func(sv replay.Served) { seen = append(seen, sv) }))
			if err != nil {
				t.Fatal(err)
			}
			ran := 0
			var tools []agenttool.Tool
			if tt.tools {
				tools = replay.Tools(orig, []agenttool.Tool{upperTool(&ran)}, replay.Strict())
			}
			s, end, err := rerun(t, fixtureConfig(model, tools...), tt.prompts...)
			if err != nil {
				t.Fatal(err)
			}
			if end.Reason != agentturn.ReasonDone {
				t.Errorf("reason = %s", end.Reason)
			}
			if model.Served() != model.Steps() {
				t.Errorf("served %d of %d steps", model.Served(), model.Steps())
			}
			if ran != 0 {
				t.Errorf("the real tool ran %d time(s)", ran)
			}
			for _, sv := range seen {
				if sv.Kind == replay.KindResponse && !sv.Match {
					t.Errorf("step %d: recorded %s, got %s", sv.N, sv.Recorded, sv.Got)
				}
			}
			want, got := hashes(orig), hashes(s)
			if strings.Join(want, ",") != strings.Join(got, ",") {
				t.Errorf("replayed hashes\n got %v\nwant %v", got, want)
			}
			// The replayed session carries the recorded response IDs
			// and usage, so it is the same run twice over.
			var origResp, newResp []*agentsession.ResponseEntry
			for _, e := range orig.Path(orig.Leaf()) {
				if r, ok := e.(*agentsession.ResponseEntry); ok {
					origResp = append(origResp, r)
				}
			}
			for _, e := range s.Path(s.Leaf()) {
				if r, ok := e.(*agentsession.ResponseEntry); ok {
					newResp = append(newResp, r)
				}
			}
			for i := range origResp {
				if newResp[i].ResponseID != origResp[i].ResponseID || newResp[i].Usage.TotalTokens != origResp[i].Usage.TotalTokens {
					t.Errorf("response %d: id %s usage %d, want %s %d", i, newResp[i].ResponseID, newResp[i].Usage.TotalTokens, origResp[i].ResponseID, origResp[i].Usage.TotalTokens)
				}
			}
		})
	}
}

func TestStrictDivergesAtTheChangedCall(t *testing.T) {
	orig := loadFixture(t, "multi")
	model, err := replay.NewModel(orig, replay.Strict())
	if err != nil {
		t.Fatal(err)
	}
	cfg := fixtureConfig(model)
	cfg.Instructions = "Be verbose."
	_, end, err := rerun(t, cfg, "first")
	if !errors.Is(err, replay.ErrDiverged) {
		t.Fatalf("err = %v, want ErrDiverged", err)
	}
	if end == nil || end.Reason != agentturn.ReasonError {
		t.Errorf("end = %+v", end)
	}
	// The error names the entry and both hashes.
	var entry, first string
	for _, e := range orig.Path(orig.Leaf()) {
		if r, ok := e.(*agentsession.ResponseEntry); ok {
			entry, first = r.ID, r.RequestHash
			break
		}
	}
	msg := err.Error()
	if !strings.Contains(msg, entry) || !strings.Contains(msg, first) || !strings.Contains(msg, "step 1") {
		t.Errorf("error does not name the entry, the step and the recorded hash: %s", msg)
	}
	if model.Served() != 1 {
		t.Errorf("served = %d, want 1", model.Served())
	}
}

// A response entry carries no request hash when the path the recorder
// wrote does not rebuild that request's input: a fold that was never
// reported, or a transform or a hook that edits the request. Strict
// replay serves such a call by position, as serveFold already does for
// a compaction entry that recorded no fold hash, because an absent
// hash says the record cannot describe the request and not that the
// request differs.
func TestStrictServesAResponseThatRecordedNoHash(t *testing.T) {
	orig := loadFixture(t, "multi")
	var blanked string
	for _, e := range orig.Path(orig.Leaf()) {
		if r, ok := e.(*agentsession.ResponseEntry); ok {
			blanked, r.RequestHash = r.ID, ""
			break
		}
	}
	if blanked == "" {
		t.Fatal("the fixture records no response")
	}
	var seen []replay.Served
	model, err := replay.NewModel(orig, replay.Strict(), replay.WithObserver(func(sv replay.Served) { seen = append(seen, sv) }))
	if err != nil {
		t.Fatal(err)
	}
	_, end, err := rerun(t, fixtureConfig(model), "first", "second")
	if err != nil {
		t.Fatalf("the recorded configuration was refused: %v", err)
	}
	if end.Reason != agentturn.ReasonDone {
		t.Errorf("reason = %s", end.Reason)
	}
	if model.Served() != model.Steps() {
		t.Errorf("served %d of %d steps", model.Served(), model.Steps())
	}
	// The unhashed call reports what a fold with no fold hash reports:
	// both hashes empty and Match false. Every other call still matches.
	for _, sv := range seen {
		if sv.EntryID == blanked {
			if sv.Recorded != "" || sv.Got != "" || sv.Match {
				t.Errorf("the unhashed call reports %+v, want both hashes empty and Match false", sv)
			}
			continue
		}
		if !sv.Match {
			t.Errorf("step %d: recorded %s, got %s", sv.N, sv.Recorded, sv.Got)
		}
	}
}

func TestLenientServesByPositionAndReports(t *testing.T) {
	orig := loadFixture(t, "multi")
	var seen []replay.Served
	model, err := replay.NewModel(orig, replay.WithObserver(func(sv replay.Served) { seen = append(seen, sv) }))
	if err != nil {
		t.Fatal(err)
	}
	cfg := fixtureConfig(model)
	cfg.Instructions = "Be verbose."
	s, end, err := rerun(t, cfg, "first", "second")
	if err != nil {
		t.Fatal(err)
	}
	if end.Reason != agentturn.ReasonDone {
		t.Errorf("reason = %s", end.Reason)
	}
	if len(seen) != 2 || seen[0].Match || seen[1].Match {
		t.Errorf("observed = %+v", seen)
	}
	// The answers are the recorded ones, whatever was asked.
	cx, err := s.Context()
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, item := range cx.Items {
		if m, ok := item.(*openresponses.Message); ok && m.Role == openresponses.RoleAssistant {
			texts = append(texts, m.Text())
		}
	}
	if strings.Join(texts, "|") != "first|second" {
		t.Errorf("assistant texts = %v", texts)
	}
}

func TestExhausted(t *testing.T) {
	orig := loadFixture(t, "multi")
	model, err := replay.NewModel(orig)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = rerun(t, fixtureConfig(model), "first", "second", "third")
	if !errors.Is(err, replay.ErrExhausted) {
		t.Fatalf("err = %v, want ErrExhausted", err)
	}
	model.Reset()
	if model.Served() != 0 {
		t.Errorf("served after reset = %d", model.Served())
	}
	if _, _, err := rerun(t, fixtureConfig(model), "first"); err != nil {
		t.Errorf("after reset: %v", err)
	}
}

func TestWithLeafChoosesThePath(t *testing.T) {
	orig := loadFixture(t, "roots")
	var firstLeaf string
	for _, leaf := range orig.Leaves() {
		if leaf != orig.Leaf() {
			firstLeaf = leaf
		}
	}
	if firstLeaf == "" {
		t.Fatal("fixture has one leaf")
	}
	tests := []struct {
		name   string
		opts   []replay.Option
		prompt string
	}{
		{"current leaf", nil, "goodbye"},
		{"named leaf", []replay.Option{replay.WithLeaf(firstLeaf)}, "hello"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model, err := replay.NewModel(orig, append(tt.opts, replay.Strict())...)
			if err != nil {
				t.Fatal(err)
			}
			if model.Steps() != 1 {
				t.Errorf("steps = %d", model.Steps())
			}
			if _, _, err := rerun(t, fixtureConfig(model), tt.prompt); err != nil {
				t.Errorf("replay: %v", err)
			}
		})
	}
	if _, err := replay.NewModel(orig, replay.WithLeaf("nope")); !errors.Is(err, agentsession.ErrNoEntry) {
		t.Errorf("unknown leaf: err = %v", err)
	}
	if _, err := replay.NewModel(agentsession.New(agentsession.Header{})); err == nil {
		t.Error("empty session: no error")
	}
}

func TestCompactedRunReplays(t *testing.T) {
	tests := []struct {
		fixture string
		// transform builds the configuration's fold over the model,
		// reporting each fold to the recorder as the fixture did.
		transform func(*replay.Model, *session.Recorder) *compact.Transform
	}{
		{"compaction", func(m *replay.Model, rec *session.Recorder) *compact.Transform {
			return compact.NewLocal(m, compact.WithBudget(1), compact.WithKeepLast(2), compact.WithOnFold(rec.Fold))
		}},
		{"endpoint", func(m *replay.Model, rec *session.Recorder) *compact.Transform {
			return compact.New(m, compact.WithBudget(1), compact.WithKeepLast(2), compact.WithOnFold(rec.Fold))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			orig := loadFixture(t, tt.fixture)
			var seen []replay.Served
			model, err := replay.NewModel(orig, replay.Strict(), replay.WithObserver(func(sv replay.Served) { seen = append(seen, sv) }))
			if err != nil {
				t.Fatal(err)
			}
			ran := 0
			cfg := fixtureConfig(model, replay.Tools(orig, []agenttool.Tool{upperTool(&ran)}, replay.Strict())...)
			s, _, err := rerunWith(t, cfg, func(cfg *agentturn.Config, rec *session.Recorder) {
				cfg.Transform = tt.transform(model, rec).Transform
			}, "one", "two", "three")
			if err != nil {
				t.Fatal(err)
			}
			folds, responses := 0, 0
			for _, sv := range seen {
				switch sv.Kind {
				case replay.KindFold:
					folds++
				case replay.KindResponse:
					responses++
					if !sv.Match {
						t.Errorf("step %d diverged", sv.N)
					}
				}
			}
			if folds == 0 || folds+responses != model.Steps() || model.Served() != model.Steps() {
				t.Errorf("folds %d responses %d of %d steps, served %d", folds, responses, model.Steps(), model.Served())
			}
			if ran != 0 {
				t.Errorf("the real tool ran %d time(s)", ran)
			}
			if want, got := hashes(orig), hashes(s); strings.Join(want, ",") != strings.Join(got, ",") {
				t.Errorf("replayed hashes\n got %v\nwant %v", got, want)
			}
		})
	}
}

func TestFoldWhereTheRecordingDidNot(t *testing.T) {
	// The recording folded before its second call; a configuration
	// without the transform sends a turn there, and strict replay says
	// so rather than serving the fold as an answer.
	orig := loadFixture(t, "compaction")
	model, err := replay.NewModel(orig, replay.Strict())
	if err != nil {
		t.Fatal(err)
	}
	ran := 0
	_, _, err = rerun(t, fixtureConfig(model, replay.Tools(orig, []agenttool.Tool{upperTool(&ran)})...), "one")
	if !errors.Is(err, replay.ErrDiverged) || !strings.Contains(err.Error(), "compaction") {
		t.Fatalf("err = %v", err)
	}
	// And a compaction request where the recording made a model call.
	model.Reset()
	if _, err := model.Compact(context.Background(), openresponses.CompactRequest{}); !errors.Is(err, replay.ErrDiverged) {
		t.Errorf("Compact at a response: err = %v", err)
	}
}

func TestCreateFoldsTheStream(t *testing.T) {
	orig := loadFixture(t, "basic")
	model, err := replay.NewModel(orig)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := model.Create(context.Background(), openresponses.Request{Model: "x", Input: openresponses.Items{openresponses.UserText("hello world")}})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.FunctionCalls()) != 1 || resp.FunctionCalls()[0].Name != "upper" {
		t.Errorf("first call output = %+v", resp.Output)
	}
	if resp.Status != openresponses.ResponseStatusCompleted || resp.Usage == nil {
		t.Errorf("response = status %s usage %v", resp.Status, resp.Usage)
	}
}

func TestDefaultFoldText(t *testing.T) {
	tests := []struct {
		name string
		item openresponses.Item
		want string
	}{
		{"summary message", compact.SummaryMessage("the gist"), "the gist"},
		{"plain message", openresponses.UserText("other"), "other"},
		{"compaction item", &openresponses.Compaction{EncryptedContent: "abc"}, "abc"},
		{"call", &openresponses.FunctionCall{Name: "x"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := replay.DefaultFoldText(tt.item); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// TestStrictChecksTheFold is issue 2: a configuration whose fold
// differs from the recorded one is refused, rather than answered with
// the recorded summary and signed off as having changed nothing.
func TestStrictChecksTheFold(t *testing.T) {
	const haiku = "Write a haiku about the conversation so far."
	tests := []struct {
		name     string
		prompt   string
		strict   bool
		diverges bool
	}{
		{"the recorded fold", "", true, false},
		{"another summary prompt, strict", haiku, true, true},
		{"another summary prompt, lenient", haiku, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			orig := loadFixture(t, "compaction")
			var seen []replay.Served
			opts := []replay.Option{replay.WithObserver(func(sv replay.Served) { seen = append(seen, sv) })}
			if tt.strict {
				opts = append(opts, replay.Strict())
			}
			model, err := replay.NewModel(orig, opts...)
			if err != nil {
				t.Fatal(err)
			}
			ran := 0
			cfg := fixtureConfig(model, replay.Tools(orig, []agenttool.Tool{upperTool(&ran)})...)
			_, _, err = rerunWith(t, cfg, func(cfg *agentturn.Config, rec *session.Recorder) {
				copts := []compact.Option{compact.WithBudget(1), compact.WithKeepLast(2), compact.WithOnFold(rec.Fold)}
				if tt.prompt != "" {
					copts = append(copts, compact.WithSummaryPrompt(tt.prompt))
				}
				cfg.Transform = compact.NewLocal(model, copts...).Transform
			}, "one", "two", "three")

			var folds, mismatched int
			var first replay.Served
			for _, sv := range seen {
				if sv.Kind != replay.KindFold {
					continue
				}
				folds++
				if sv.Recorded == "" {
					t.Errorf("fold at step %d reports no recorded hash", sv.N)
				}
				if !sv.Match {
					mismatched++
					if first.Recorded == "" {
						first = sv
					}
				}
			}
			if folds == 0 {
				t.Fatal("no fold was served")
			}
			if !tt.diverges {
				if err != nil {
					t.Fatalf("replay: %v", err)
				}
				if tt.prompt == "" && mismatched != 0 {
					t.Errorf("%d of %d folds mismatched under the recorded prompt", mismatched, folds)
				}
				if tt.prompt != "" && mismatched == 0 {
					t.Error("a lenient replay reported no mismatch although the fold's prompt changed")
				}
				return
			}
			if !errors.Is(err, replay.ErrDiverged) {
				t.Fatalf("err = %v, want ErrDiverged", err)
			}
			if first.Got == "" || first.Got == first.Recorded {
				t.Errorf("the observer did not report both hashes: %+v", first)
			}
			msg := err.Error()
			for _, want := range []string{"compaction", "fold", first.EntryID, first.Recorded, first.Got} {
				if !strings.Contains(msg, want) {
					t.Errorf("the error does not name %q: %s", want, msg)
				}
			}
		})
	}
}

// layerInstructions is what the layers fixture's product renders from
// the state it has written so far, the same string the fixture was
// recorded with.
func layerInstructions(notes int) string {
	if notes == 0 {
		return "Be brief."
	}
	return fmt.Sprintf("Be brief. Notes: %d", notes)
}

// layeredConfig is that product: its instructions are rebuilt before
// every call from state the run itself writes, as a memory block or a
// skill set is rebuilt. notes says how far that state had already
// moved on when the replay started, freeze holds the instructions at
// one string for the whole run instead of re-rendering them, and
// chain hands the request on to the model's own hook.
func layeredConfig(model *replay.Model, notes int, freeze, chain bool) agentturn.Config {
	cfg := fixtureConfig(model)
	cfg.BeforeModelCall = func(ctx context.Context, req *openresponses.Request) error {
		req.Instructions = layerInstructions(notes)
		if chain {
			return model.BeforeModelCall(ctx, req)
		}
		return nil
	}
	if !freeze {
		cfg.AfterToolCall = func(context.Context, agentturn.ToolResultInfo) (*agentturn.ToolOverride, error) {
			notes++
			return nil, nil
		}
	}
	return cfg
}

// TestReplayServesTheRecordedSettings is issue 5: a product whose
// layers re-read state each turn replays strictly only when the
// settings in force at each recorded call are served back to it.
func TestReplayServesTheRecordedSettings(t *testing.T) {
	tests := []struct {
		name   string
		notes  int
		freeze bool
		chain  bool
		step   int // the step it diverges at; 0 when it replays whole
	}{
		{"the layer as it stands, its state moved on", 1, false, false, 1},
		{"the instructions frozen from the first config entry", 0, true, false, 2},
		{"the settings in force at each recorded call", 1, false, true, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			orig := loadFixture(t, "layers")
			var seen []replay.Served
			model, err := replay.NewModel(orig, replay.Strict(), replay.WithObserver(func(sv replay.Served) { seen = append(seen, sv) }))
			if err != nil {
				t.Fatal(err)
			}
			ran := 0
			cfg := layeredConfig(model, tt.notes, tt.freeze, tt.chain)
			cfg.Tools = replay.Tools(orig, []agenttool.Tool{upperTool(&ran)}, replay.Strict())
			_, end, err := rerun(t, cfg, "hello world")
			if tt.step == 0 {
				if err != nil {
					t.Fatalf("replay: %v", err)
				}
				if end.Reason != agentturn.ReasonDone {
					t.Errorf("reason = %s", end.Reason)
				}
				if model.Served() != model.Steps() {
					t.Errorf("served %d of %d steps", model.Served(), model.Steps())
				}
				for _, sv := range seen {
					if !sv.Match {
						t.Errorf("step %d: recorded %s, got %s", sv.N, sv.Recorded, sv.Got)
					}
				}
				return
			}
			if !errors.Is(err, replay.ErrDiverged) {
				t.Fatalf("err = %v, want ErrDiverged", err)
			}
			if len(seen) != tt.step || !strings.Contains(err.Error(), fmt.Sprintf("step %d", tt.step)) {
				t.Errorf("diverged at step %d of %d served: %v", tt.step, len(seen), err)
			}
		})
	}
}

// TestModelSettings is what a judge reads: the settings in force at
// each recorded call, in step order.
func TestModelSettings(t *testing.T) {
	model, err := replay.NewModel(loadFixture(t, "layers"))
	if err != nil {
		t.Fatal(err)
	}
	settings := model.Settings()
	if len(settings) != model.Steps() || model.Steps() != 2 {
		t.Fatalf("%d settings for %d steps", len(settings), model.Steps())
	}
	for i, want := range []string{layerInstructions(0), layerInstructions(1)} {
		if settings[i].Instructions != want {
			t.Errorf("step %d instructions = %q, want %q", i+1, settings[i].Instructions, want)
		}
		at, ok := model.SettingsAt(i + 1)
		if !ok || at.Instructions != want || at.Model != "echo/echo-1" || len(at.Tools) != 1 {
			t.Errorf("SettingsAt(%d) = %+v %v", i+1, at, ok)
		}
	}
	if _, ok := model.SettingsAt(0); ok {
		t.Error("SettingsAt(0) reported a step")
	}
	if _, ok := model.SettingsAt(model.Steps() + 1); ok {
		t.Error("SettingsAt past the end reported a step")
	}
}

// itemJSON is an item as the wire carries it, which is what a
// recording holds and what a replay must serve back.
func itemJSON(t *testing.T, item openresponses.Item) string {
	t.Helper()
	data, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// TestReplayServesItemsVerbatim is issue 6: a recorded item is served
// as it was recorded. The emitter promotes an unset status to
// completed, and a server that leaves one unset, as Ollama does on a
// reasoning item, would otherwise record a session that can never
// replay: the added field changes every later request.
func TestReplayServesItemsVerbatim(t *testing.T) {
	tests := []struct {
		name string
		item openresponses.Item
	}{
		{"a reasoning item the server sent no status for", &openresponses.ReasoningItem{
			ID: "rs_1", Summary: openresponses.Contents{&openresponses.SummaryText{Text: "thinking"}},
		}},
		{"a message with the status the server sent", &openresponses.Message{
			ID: "msg_1", Status: openresponses.StatusCompleted, Role: openresponses.RoleAssistant,
			Content: openresponses.Contents{&openresponses.OutputText{Text: "hello"}},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := agentsession.New(agentsession.Header{})
			req := openresponses.Request{Model: "m", Instructions: "be brief", Input: openresponses.Items{openresponses.UserText("hello")}}
			cfg, err := agentsession.ConfigFromRequest(req)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range []agentsession.Entry{cfg, agentsession.NewItemEntry(openresponses.UserText("hello"))} {
				if _, err := s.Append(e); err != nil {
					t.Fatal(err)
				}
			}
			item := agentsession.NewItemEntry(tt.item)
			item.ResponseID = "resp_1"
			if _, err := s.Append(item); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Append(&agentsession.ResponseEntry{ResponseID: "resp_1", Model: "m", Status: openresponses.ResponseStatusCompleted}); err != nil {
				t.Fatal(err)
			}
			model, err := replay.NewModel(s)
			if err != nil {
				t.Fatal(err)
			}
			got, err := model.Create(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Output) != 1 {
				t.Fatalf("served %d items", len(got.Output))
			}
			if want, served := itemJSON(t, tt.item), itemJSON(t, got.Output[0]); served != want {
				t.Errorf("served\n got %s\nwant %s", served, want)
			}
			if got.Status != openresponses.ResponseStatusCompleted {
				t.Errorf("response status = %s", got.Status)
			}
		})
	}
}

// TestInterleavedCustomEntry is the contiguity rule of seam 1: an
// entry that is not an item entry between two output items of one
// response is skipped, and the walk stops at the first item entry
// belonging to something else. An OutputGuard observer that writes its
// verdict there, which is where every layer above the loop writes one,
// otherwise costs the response the items before it.
func TestInterleavedCustomEntry(t *testing.T) {
	first := &openresponses.ReasoningItem{ID: "rs_1", Summary: openresponses.Contents{&openresponses.SummaryText{Text: "thinking"}}}
	second := &openresponses.Message{
		ID: "msg_1", Status: openresponses.StatusCompleted, Role: openresponses.RoleAssistant,
		Content: openresponses.Contents{&openresponses.OutputText{Text: "hello"}},
	}
	s := agentsession.New(agentsession.Header{})
	req := openresponses.Request{Model: "m", Input: openresponses.Items{openresponses.UserText("hi")}}
	cfg, err := agentsession.ConfigFromRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	output := func(item openresponses.Item) agentsession.Entry {
		e := agentsession.NewItemEntry(item)
		e.ResponseID = "resp_1"
		return e
	}
	entries := []agentsession.Entry{
		cfg,
		agentsession.NewItemEntry(openresponses.UserText("hi")),
		output(first),
		// The verdict of a guard, written between the two items of one
		// response.
		&agentsession.CustomEntry{NS: "product:guard", Data: []byte(`{"verdict":"allow"}`)},
		output(second),
		&agentsession.ResponseEntry{ResponseID: "resp_1", Model: "m", Status: openresponses.ResponseStatusCompleted},
	}
	for _, e := range entries {
		if _, err := s.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	model, err := replay.NewModel(s)
	if err != nil {
		t.Fatal(err)
	}
	got, err := model.Create(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Output) != 2 {
		t.Fatalf("served %d of the response's 2 output items", len(got.Output))
	}
	for i, want := range []openresponses.Item{first, second} {
		if served := itemJSON(t, got.Output[i]); served != itemJSON(t, want) {
			t.Errorf("item %d served %s", i, served)
		}
	}
	// The user message before the response is not its output, whatever
	// stands between them.
	for _, item := range got.Output {
		if m, ok := item.(*openresponses.Message); ok && m.Role == openresponses.RoleUser {
			t.Error("an input item was served as output")
		}
	}
}
