package judge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/ChristopherDavenport/agenteval"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/export"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/session"
	"github.com/ChristopherDavenport/openresponses"
)

// Verdict is the schema a rubric judge answers in.
type Verdict struct {
	Score  float64 `json:"score"`
	Pass   bool    `json:"pass"`
	Reason string  `json:"reason"`
}

// VerdictSchema is the JSON Schema of [Verdict], set as the judge's
// text format so the model answers in it.
const VerdictSchema = `{"type":"object","properties":{"score":{"type":"number","description":"The score under the rubric."},"pass":{"type":"boolean","description":"Whether the run passes under the rubric."},"reason":{"type":"string","description":"One paragraph saying why."}},"required":["score","pass","reason"],"additionalProperties":false}`

// ErrNoVerdict is returned when the judge's run ended without a final
// message in the verdict schema.
var ErrNoVerdict = errors.New("judge: the rubric judge gave no verdict")

// Option configures [Rubric].
type Option func(*rubric)

// WithStore makes the judge record its own run as a session in store,
// with the judged session as its parent, and name it on the score.
func WithStore(store agentsession.Store) Option { return func(r *rubric) { r.store = store } }

// WithName names the judge; the default is "rubric".
func WithName(name string) Option { return func(r *rubric) { r.name = name } }

// WithRender replaces how the trajectory becomes the judge's input.
// The default is [Render].
func WithRender(fn func(export.Trajectory) (string, error)) Option {
	return func(r *rubric) { r.render = fn }
}

// WithHarness names the writer in the judge's session header.
func WithHarness(name, version string) Option {
	return func(r *rubric) { r.harness = &agentsession.Harness{Name: name, Version: version} }
}

type rubric struct {
	cfg     agentturn.Config
	name    string
	store   agentsession.Store
	render  func(export.Trajectory) (string, error)
	harness *agentsession.Harness
}

// Rubric returns a judge that is an agent: cfg with the rubric as its
// instructions and its output constrained to [VerdictSchema], prompted
// with the rendered trajectory. It runs under the loop like any agent,
// tools included, and with [WithStore] records its own session, so a
// judgement is as replayable as the run it judged. The verdict's score
// is the judge's Value and the whole verdict is kept as Details.
func Rubric(cfg agentturn.Config, instructions string, opts ...Option) agenteval.Judge {
	r := &rubric{cfg: cfg, name: "rubric", render: Render}
	for _, opt := range opts {
		opt(r)
	}
	r.cfg.Instructions = instructions
	r.cfg.Text.Format = openresponses.JSONSchemaFormat("verdict", json.RawMessage(VerdictSchema), true)
	return r
}

func (r *rubric) Name() string { return r.name }

func (r *rubric) Judge(ctx context.Context, t export.Trajectory, task agenteval.Task) (agenteval.Score, error) {
	input, err := r.render(t)
	if err != nil {
		return agenteval.Score{}, fmt.Errorf("judge: render trajectory: %w", err)
	}
	a := agentturn.New(r.cfg)
	sessionID := ""
	if r.store != nil {
		rec, s, err := session.Start(ctx, r.store, agentsession.Header{ParentSession: t.Header.ID, Harness: r.harness})
		if err != nil {
			return agenteval.Score{}, fmt.Errorf("judge: start session: %w", err)
		}
		sessionID = s.ID()
		defer rec.Attach(a)()
		if _, err := r.store.Append(ctx, sessionID, &agentsession.InfoEntry{Name: r.name + ": " + task.ID}); err != nil {
			return agenteval.Score{}, fmt.Errorf("judge: %w", err)
		}
	}
	end, err := a.Prompt(ctx, openresponses.UserText(input))
	if err != nil {
		return agenteval.Score{}, fmt.Errorf("judge: %w", err)
	}
	text := ""
	for i := len(end.Items) - 1; i >= 0; i-- {
		if m, ok := end.Items[i].(*openresponses.Message); ok && m.Role == openresponses.RoleAssistant {
			text = m.Text()
			break
		}
	}
	if strings.TrimSpace(text) == "" {
		return agenteval.Score{}, fmt.Errorf("%w: run ended %s", ErrNoVerdict, end.Reason)
	}
	// Unknown members are refused so that a model echoing something
	// else back, itself valid JSON, is not read as a zero verdict.
	dec := json.NewDecoder(strings.NewReader(text))
	dec.DisallowUnknownFields()
	var v Verdict
	if err := dec.Decode(&v); err != nil {
		return agenteval.Score{}, fmt.Errorf("%w: %v in %q", ErrNoVerdict, err, text)
	}
	details, err := json.Marshal(v)
	if err != nil {
		return agenteval.Score{}, err
	}
	return agenteval.Score{Judge: r.name, Value: v.Score, Pass: v.Pass, Reason: v.Reason, Details: details, Session: sessionID}, nil
}

// Render is the default input of a rubric judge: the trajectory's ATIF
// document with the raw item passthrough stripped by
// [export.NoPassthrough], indented. The passthrough is about half a
// document and says nothing the declared fields do not, and a judge's
// context is better spent once.
func Render(t export.Trajectory) (string, error) {
	doc, err := export.ToATIF(t, export.Options{Redactors: []export.Redactor{export.NoPassthrough()}})
	if err != nil {
		return "", err
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// StripRaw drops the raw Open Responses items the exporter carries
// under extra.openresponses, which the design study measured at about
// half a document and which a judge's context is better spent once.
//
// Deprecated: it is [export.NoPassthrough], which does the same work
// and keeps the root's payload profile name, so a judged document
// still says which wire profile produced it. This is that function
// under the name this package gave it; [Render] calls NoPassthrough
// directly.
func StripRaw() export.Redactor { return export.NoPassthrough() }
