package replay_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/ChristopherDavenport/agenteval/replay"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agenttool"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/session"
	"github.com/ChristopherDavenport/openresponses"
)

// rawType is an extension item type agentturn's DefaultFilter keeps
// from the model, as a text-call parser's raw item is.
const rawType = "test:raw"

// rawItem is a rawType item carrying text.
func rawItem(text string) *openresponses.UnknownItem {
	data, _ := json.Marshal(map[string]string{"type": rawType, "text": text})
	return &openresponses.UnknownItem{Type: rawType, Raw: data}
}

// marked is the custom entry agentturn/session v0.0.15 writes for a
// model output the filter kept from the model: the item as data, in the
// namespace of its type, marked with the response that produced it.
func marked(t *testing.T, item openresponses.Item, responseID string) *agentsession.CustomEntry {
	t.Helper()
	data, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	id, err := json.Marshal(responseID)
	if err != nil {
		t.Fatal(err)
	}
	return &agentsession.CustomEntry{NS: item.ItemType(), Data: data, EntryBase: agentsession.EntryBase{Unknown: map[string]json.RawMessage{session.ResponseIDMember: id}}}
}

// Issue 50: a response's output is the item entries before it naming
// its response ID and, since agentturn v0.0.15, the custom entries
// marked with it, which hold an output item the filter kept from the
// model. Both are served, in their order; a marked entry of another
// response, or of an app-only input, belongs to something else and ends
// the walk as a foreign item entry does.
func TestMarkedOutputItemsAreServed(t *testing.T) {
	reasoning := &openresponses.ReasoningItem{ID: "rs_1", Summary: openresponses.Contents{&openresponses.SummaryText{Text: "thinking"}}}
	msg := &openresponses.Message{
		ID: "msg_1", Status: openresponses.StatusCompleted, Role: openresponses.RoleAssistant,
		Content: openresponses.Contents{&openresponses.OutputText{Text: "hello"}},
	}
	raw := rawItem("<thinking>hello</thinking>")
	output := func(item openresponses.Item) agentsession.Entry {
		e := agentsession.NewItemEntry(item)
		e.ResponseID = "resp_1"
		return e
	}
	tests := []struct {
		name    string
		entries func(t *testing.T) []agentsession.Entry
		want    []openresponses.Item
	}{
		{"a marked item between two item entries", func(t *testing.T) []agentsession.Entry {
			return []agentsession.Entry{output(reasoning), marked(t, raw, "resp_1"), output(msg)}
		}, []openresponses.Item{reasoning, raw, msg}},
		{"a marked item first", func(t *testing.T) []agentsession.Entry {
			return []agentsession.Entry{marked(t, raw, "resp_1"), output(msg)}
		}, []openresponses.Item{raw, msg}},
		{"a marked item last", func(t *testing.T) []agentsession.Entry {
			return []agentsession.Entry{output(msg), marked(t, raw, "resp_1")}
		}, []openresponses.Item{msg, raw}},
		{"a marked item of another response ends the walk", func(t *testing.T) []agentsession.Entry {
			return []agentsession.Entry{marked(t, raw, "resp_0"), output(msg)}
		}, []openresponses.Item{msg}},
		{"a marked app-only input ends the walk", func(t *testing.T) []agentsession.Entry {
			return []agentsession.Entry{marked(t, raw, ""), output(msg)}
		}, []openresponses.Item{msg}},
		{"an unmarked custom entry is skipped", func(t *testing.T) []agentsession.Entry {
			return []agentsession.Entry{output(reasoning), &agentsession.CustomEntry{NS: "product:guard", Data: []byte(`{"verdict":"allow"}`)}, output(msg)}
		}, []openresponses.Item{reasoning, msg}},
		{"a marked entry whose namespace is not its item's type is skipped", func(t *testing.T) []agentsession.Entry {
			c := marked(t, raw, "resp_1")
			c.NS = "test:other"
			return []agentsession.Entry{output(reasoning), c, output(msg)}
		}, []openresponses.Item{reasoning, msg}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := agentsession.New(agentsession.Header{})
			req := openresponses.Request{Model: "m", Input: openresponses.Items{openresponses.UserText("hi")}}
			cfg, err := agentsession.ConfigFromRequest(req)
			if err != nil {
				t.Fatal(err)
			}
			entries := []agentsession.Entry{cfg, agentsession.NewItemEntry(openresponses.UserText("hi"))}
			entries = append(entries, tt.entries(t)...)
			entries = append(entries, &agentsession.ResponseEntry{ResponseID: "resp_1", Model: "m", Status: openresponses.ResponseStatusCompleted})
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
			if len(got.Output) != len(tt.want) {
				t.Fatalf("served %d items, want %d: %s", len(got.Output), len(tt.want), itemsJSON(t, got.Output))
			}
			for i, want := range tt.want {
				if served := itemJSON(t, got.Output[i]); served != itemJSON(t, want) {
					t.Errorf("item %d served %s, want %s", i, served, itemJSON(t, want))
				}
			}
		})
	}
}

func itemsJSON(t *testing.T, items openresponses.Items) string {
	t.Helper()
	data, err := json.Marshal(items)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// rawTalker answers each call with a rawType item and a message, as a
// text-call parser answers with the sampled text beside what it parsed.
type rawTalker struct{}

func (rawTalker) CreateStream(_ context.Context, req openresponses.Request, sink openresponses.EventSink) error {
	em := openresponses.NewEmitter(sink, openresponses.NewResponse(req))
	if err := em.Item(rawItem("raw: ok")); err != nil {
		return err
	}
	if err := em.Item(openresponses.AssistantText("ok")); err != nil {
		return err
	}
	return em.Complete()
}

// Issue 50, as recorded: a run under agentturn's DefaultFilter, which
// keeps a slug-prefixed item from the model, records that item as a
// marked custom entry, and a strict replay serves it with the rest of
// the response. The filter keeps it out of every request, so nothing
// else would say the replayed output was short of it.
func TestStrictServesAFilteredOutput(t *testing.T) {
	cfg := fixtureConfig(rawTalker{})
	cfg.Tools = nil
	orig, end, err := rerun(t, cfg, "first", "second")
	if err != nil {
		t.Fatal(err)
	}
	live := countType(end.Items, rawType)
	markedEntries := 0
	for _, e := range orig.Path(orig.Leaf()) {
		if c, ok := e.(*agentsession.CustomEntry); ok && c.NS == rawType {
			markedEntries++
		}
	}
	if live != 1 || markedEntries != 2 {
		t.Fatalf("the recording's last run produced %d raw items and the path holds %d marked entries; want 1 and 2", live, markedEntries)
	}
	model, err := replay.NewModel(orig, replay.Strict())
	if err != nil {
		t.Fatal(err)
	}
	cfg = fixtureConfig(model)
	cfg.Tools = nil
	s, rend, err := rerun(t, cfg, "first", "second")
	if err != nil {
		t.Fatalf("strict replay: %v", err)
	}
	if rend.Reason != agentturn.ReasonDone || model.Served() != model.Steps() {
		t.Errorf("reason %s, served %d of %d", rend.Reason, model.Served(), model.Steps())
	}
	if got := countType(rend.Items, rawType); got != live {
		t.Errorf("the replayed run produced %d raw items, the recording %d", got, live)
	}
	// The replayed run's record marks them as the recording did.
	replayed := 0
	for _, e := range s.Path(s.Leaf()) {
		if c, ok := e.(*agentsession.CustomEntry); ok && c.NS == rawType {
			replayed++
		}
	}
	if replayed != markedEntries {
		t.Errorf("the replayed session holds %d marked entries, the recording %d", replayed, markedEntries)
	}
}

func countType(items openresponses.Items, typ string) int {
	n := 0
	for _, it := range items {
		if it.ItemType() == typ {
			n++
		}
	}
	return n
}

// Issue 50, for Tools: a function call's output a filter kept from the
// model is a marked custom entry too, and the call is served from it.
func TestToolsServeAMarkedOutput(t *testing.T) {
	s := agentsession.New(agentsession.Header{})
	call := &openresponses.FunctionCall{ID: "fc_1", CallID: "call_1", Name: "upper", Arguments: `{"text":"hi"}`, Status: openresponses.StatusCompleted}
	out := openresponses.NewFunctionCallOutput("call_1", "HI")
	entries := []agentsession.Entry{
		agentsession.NewItemEntry(openresponses.UserText("hi")),
		agentsession.NewItemEntry(call),
		marked(t, out, ""),
	}
	for _, e := range entries {
		if _, err := s.Append(e); err != nil {
			t.Fatal(err)
		}
	}
	ran := 0
	tools := replay.Tools(s, []agenttool.Tool{upperTool(&ran)}, replay.Strict())
	res, err := tools[0].Execute(context.Background(), agenttool.Call{ID: "call_1", Args: json.RawMessage(`{"text":"hi"}`)})
	if err != nil {
		t.Fatalf("Execute = %v, want the recorded output", err)
	}
	if res.Output.String() != "HI" || ran != 0 {
		t.Errorf("served %q and ran the tool %d times; want HI from the record", res.Output.String(), ran)
	}
}
