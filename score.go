package agenteval

import (
	"context"
	"encoding/json"

	"github.com/ChristopherDavenport/agentsession/export"
)

// Score is one judge's verdict on one run.
type Score struct {
	// Judge is the judge's name; it becomes the outcome entry's label.
	Judge string `json:"judge"`
	// Value is the score on the judge's own scale: comparable across
	// runs of the same judge, not normalised. The deterministic judges
	// use 0 and 1; a Harbor verifier reports whatever its test script
	// wrote. A judge that normalises keeps the raw number in Details.
	Value float64 `json:"value"`
	// Pass is the judge's verdict.
	Pass bool `json:"pass"`
	// Reason says why, in a sentence.
	Reason string `json:"reason,omitempty"`
	// Details is the judge's own record, kept under the outcome's
	// details.judge.
	Details json.RawMessage `json:"details,omitempty"`
	// Session is the ID of the session the judge recorded of its own
	// run, when it is an agent that recorded one.
	Session string `json:"session,omitempty"`
}

// Judge scores a run from its trajectory and the task it ran.
type Judge interface {
	// Name identifies the judge in scores and outcome entries.
	Name() string
	// Judge scores the trajectory. An error means the judge could not
	// reach a verdict, not that the run failed; the runner records the
	// error on the result and writes no outcome for it.
	Judge(ctx context.Context, t export.Trajectory, task Task) (Score, error)
}

// JudgeFunc adapts a function to Judge.
type JudgeFunc struct {
	JudgeName string
	Fn        func(ctx context.Context, t export.Trajectory, task Task) (Score, error)
}

// Name returns JudgeName.
func (j JudgeFunc) Name() string { return j.JudgeName }

// Judge calls Fn and stamps the score with the judge's name.
func (j JudgeFunc) Judge(ctx context.Context, t export.Trajectory, task Task) (Score, error) {
	s, err := j.Fn(ctx, t, task)
	if s.Judge == "" {
		s.Judge = j.JudgeName
	}
	return s, err
}

// GroupJudge scores the samples of one task together, once each has
// been judged by the runner's judges: an advantage relative to the
// group, or a length penalty that applies only when every sample
// passed. The runner calls it once per task, over its
// [Runner.Samples] samples, or the one run when it does not sample.
type GroupJudge interface {
	// Name identifies the judge in scores and outcome entries.
	Name() string
	// JudgeGroup returns one score per member, in the group's order.
	// An error, or a count other than the group's, means the judge
	// could not reach a verdict; the runner records the error on
	// every member's result and writes no outcome for it.
	JudgeGroup(ctx context.Context, group []Member, task Task) ([]Score, error)
}

// Member is one sample of a group: its trajectory, and what the
// runner's judges said of it.
type Member struct {
	Trajectory export.Trajectory
	Scores     []Score
}

// GroupJudgeFunc adapts a function to GroupJudge.
type GroupJudgeFunc struct {
	JudgeName string
	Fn        func(ctx context.Context, group []Member, task Task) ([]Score, error)
}

// Name returns JudgeName.
func (j GroupJudgeFunc) Name() string { return j.JudgeName }

// JudgeGroup calls Fn and stamps each score with the judge's name.
func (j GroupJudgeFunc) JudgeGroup(ctx context.Context, group []Member, task Task) ([]Score, error) {
	scores, err := j.Fn(ctx, group, task)
	for i := range scores {
		if scores[i].Judge == "" {
			scores[i].Judge = j.JudgeName
		}
	}
	return scores, err
}
