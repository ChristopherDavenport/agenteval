package agenteval

import (
	"encoding/json"

	"github.com/ChristopherDavenport/agentsession"
)

// TaskNS is the namespace of the custom entry the runner writes before
// each run, whose data is a [TaskRecord].
const TaskNS = "agenteval:task"

// TaskRecord is the data of a [TaskNS] custom entry: which suite and
// task a session ran, and the task's setup and metadata.
type TaskRecord struct {
	Suite    string            `json:"suite"`
	Location string            `json:"location,omitempty"`
	Task     string            `json:"task"`
	Setup    map[string]string `json:"setup,omitempty"`
	Meta     map[string]string `json:"meta,omitempty"`
}

// OutcomeDetails is the details member of an outcome entry the runner
// writes, so a reader gets the task, the reason and the judge's own
// session at known keys without knowing any judge's private JSON.
type OutcomeDetails struct {
	Task    string          `json:"task"`
	Reason  string          `json:"reason,omitempty"`
	Session string          `json:"session,omitempty"`
	Judge   json.RawMessage `json:"judge,omitempty"`
}

// NewOutcome builds the outcome entry for a score: kind
// [agentsession.OutcomeEval], target the entry named, score, pass and
// label from the score, and [OutcomeDetails] carrying the task. The
// entry is not appended.
func NewOutcome(score Score, target, task string) (*agentsession.OutcomeEntry, error) {
	details, err := json.Marshal(OutcomeDetails{Task: task, Reason: score.Reason, Session: score.Session, Judge: score.Details})
	if err != nil {
		return nil, err
	}
	e := agentsession.NewOutcomeEntry(agentsession.OutcomeEval, target).WithScore(score.Value).WithPass(score.Pass).WithLabel(score.Judge)
	e.Details = details
	return e, nil
}

// ReadOutcome reads a score back out of an outcome entry the runner
// wrote. It reports false for an entry of another kind or one without
// [OutcomeDetails].
func ReadOutcome(e *agentsession.OutcomeEntry) (Score, OutcomeDetails, bool) {
	if e.Kind != agentsession.OutcomeEval || len(e.Details) == 0 {
		return Score{}, OutcomeDetails{}, false
	}
	var d OutcomeDetails
	if err := json.Unmarshal(e.Details, &d); err != nil {
		return Score{}, OutcomeDetails{}, false
	}
	s := Score{Judge: e.Label, Reason: d.Reason, Details: d.Judge, Session: d.Session}
	if e.Score != nil {
		s.Value = *e.Score
	}
	if e.Pass != nil {
		s.Pass = *e.Pass
	}
	return s, d, true
}
