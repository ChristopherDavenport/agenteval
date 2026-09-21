package judge_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ChristopherDavenport/agenteval/judge"
	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/export"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/openresponses"
	"github.com/ChristopherDavenport/openresponses/echo"
)

// canned makes the echo adapter answer with the verdict: the judge's
// input is the verdict text, and echo repeats the last user message.
func canned(v judge.Verdict) func(export.Trajectory) (string, error) {
	return func(export.Trajectory) (string, error) {
		data, err := json.Marshal(v)
		return string(data), err
	}
}

func TestRubricJudge(t *testing.T) {
	judged := loadFixture(t, "basic")
	tr := mainTrajectory(t, judged)
	store := agentsession.NewMemoryStore()
	want := judge.Verdict{Score: 0.75, Pass: true, Reason: "the tool was used and the answer repeats its result"}
	j := judge.Rubric(agentturn.Config{Model: &echo.Adapter{}, ModelName: "echo/judge"}, "Score the run from 0 to 1.",
		judge.WithStore(store), judge.WithName("quality"), judge.WithRender(canned(want)), judge.WithHarness("test", "1"))
	if j.Name() != "quality" {
		t.Errorf("name = %q", j.Name())
	}
	score, err := j.Judge(context.Background(), tr, judgeTask("t1"))
	if err != nil {
		t.Fatal(err)
	}
	if score.Judge != "quality" || score.Value != 0.75 || !score.Pass || score.Reason != want.Reason {
		t.Errorf("score = %+v", score)
	}
	var got judge.Verdict
	if err := json.Unmarshal(score.Details, &got); err != nil || got != want {
		t.Errorf("details = %s (%v)", score.Details, err)
	}
	if score.Session == "" {
		t.Fatal("score names no session")
	}
	// The judge's session is a run like any other: parented to the
	// judged session, named, configured with the rubric and the
	// verdict schema, and verifiable.
	s, err := store.Open(context.Background(), score.Session)
	if err != nil {
		t.Fatal(err)
	}
	if h := s.Header(); h.ParentSession != judged.ID() || h.Harness == nil || h.Harness.Name != "test" {
		t.Errorf("header = %+v", h)
	}
	if s.Name() != "quality: t1" {
		t.Errorf("name = %q", s.Name())
	}
	cx, err := s.Context()
	if err != nil {
		t.Fatal(err)
	}
	if cx.Settings.Instructions != "Score the run from 0 to 1." || cx.Settings.Text.Format == nil || cx.Settings.Text.Format.Type != openresponses.TextFormatJSONSchema || cx.Settings.Text.Format.Name != "verdict" {
		t.Errorf("settings = %+v", cx.Settings)
	}
	for _, e := range s.Entries() {
		if _, ok := e.(*agentsession.ResponseEntry); ok {
			if err := s.Verify(e.Base().ID); err != nil {
				t.Error(err)
			}
		}
	}
	// A summary listing finds it from the judged session.
	found := false
	for sum := range store.List(context.Background(), agentsession.ListFilter{ParentSession: judged.ID()}) {
		found = found || sum.Header.ID == score.Session
	}
	if !found {
		t.Error("the judge's session is not listed under the judged session")
	}
}

func TestRubricWithoutStore(t *testing.T) {
	tr := mainTrajectory(t, loadFixture(t, "basic"))
	j := judge.Rubric(agentturn.Config{Model: &echo.Adapter{}}, "rubric", judge.WithRender(canned(judge.Verdict{Score: 2, Reason: "r"})))
	score, err := j.Judge(context.Background(), tr, judgeTask("t"))
	if err != nil {
		t.Fatal(err)
	}
	if score.Session != "" || score.Value != 2 || score.Pass || score.Judge != "rubric" {
		t.Errorf("score = %+v", score)
	}
}

func TestRubricNoVerdict(t *testing.T) {
	tr := mainTrajectory(t, loadFixture(t, "basic"))
	tests := []struct {
		name   string
		render func(export.Trajectory) (string, error)
	}{
		{"not json", func(export.Trajectory) (string, error) { return "I refuse.", nil }},
		{"other json", func(export.Trajectory) (string, error) { return `{"answer": 42}`, nil }},
		{"the document itself", judge.Render},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			j := judge.Rubric(agentturn.Config{Model: &echo.Adapter{}}, "rubric", judge.WithRender(tt.render))
			_, err := j.Judge(context.Background(), tr, judgeTask("t"))
			if !errors.Is(err, judge.ErrNoVerdict) {
				t.Errorf("err = %v, want ErrNoVerdict", err)
			}
		})
	}
	j := judge.Rubric(agentturn.Config{Model: &echo.Adapter{}}, "rubric", judge.WithRender(func(export.Trajectory) (string, error) { return "", errors.New("boom") }))
	if _, err := j.Judge(context.Background(), tr, judgeTask("t")); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("render error: %v", err)
	}
}

func TestRenderStripsRaw(t *testing.T) {
	tr := mainTrajectory(t, loadFixture(t, "basic"))
	full, err := export.ToATIF(tr, export.Options{})
	if err != nil {
		t.Fatal(err)
	}
	fullJSON, _ := json.Marshal(full)
	text, err := judge.Render(tr)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text, `"`+export.ExtraOpenResponses+`"`) {
		t.Error("rendered document still carries extra.openresponses")
	}
	if !strings.Contains(text, "HELLO WORLD") || !strings.Contains(text, `"upper"`) {
		t.Error("rendered document lost the tool call or its result")
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(text), &doc); err != nil {
		t.Fatalf("rendered document is not JSON: %v", err)
	}
	if steps, _ := doc["steps"].([]any); len(steps) != len(full.Steps) {
		t.Errorf("rendered %d steps, want %d", len(steps), len(full.Steps))
	}
	compact, _ := json.Marshal(doc)
	if len(compact) >= len(fullJSON) {
		t.Errorf("stripped document is %d bytes, full is %d", len(compact), len(fullJSON))
	}
}
