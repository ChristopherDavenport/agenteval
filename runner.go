package agenteval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentsession/export"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/session"
	"github.com/ChristopherDavenport/openresponses"
)

// Runner sends each task of a suite through a configuration, records
// the run as a session and judges it.
type Runner struct {
	// Store is where each run's session is created. Required.
	Store agentsession.Store
	// Config returns the configuration under test for a task. Required.
	Config func(Task) agentturn.Config
	// Judges score each run once it has ended. Each score is appended
	// to the run's session as an outcome entry.
	Judges []Judge
	// Parallel bounds how many tasks run at once; zero or one means one
	// at a time.
	Parallel int
	// Header, when set, supplies each run's session header: a harness
	// name, a working directory, a fixed ID for a test. Empty fields
	// are filled by the store.
	Header func(Task) agentsession.Header
	// Cost prices one model call, as export.Options.Cost does; see
	// price.Hook. When set, and every call of a run is priced, the
	// result carries the run's cost.
	Cost func(model string, usage openresponses.Usage) (float64, bool)
}

// Result is one task's run and its scores.
type Result struct {
	Task Task `json:"task"`
	// SessionID is the session the run was recorded in.
	SessionID string `json:"session_id"`
	// Target is the entry every score of this result targets: the last
	// entry of the run, before any outcome was appended.
	Target string `json:"target,omitempty"`
	// Reason is how the last run of the task ended.
	Reason agentturn.Reason `json:"reason,omitempty"`
	// Runs is how many runs of the loop the task took: one per prompt
	// sent.
	Runs int `json:"runs"`
	// Usage is the sum over the responses on the run's path.
	Usage openresponses.Usage `json:"usage"`
	// CostUSD is the run's cost under Runner.Cost, when every call was
	// priced.
	CostUSD *float64 `json:"cost_usd,omitempty"`
	// Scores are the judges' verdicts, in the runner's judge order. A
	// judge that failed is missing here and named in Err.
	Scores []Score `json:"scores"`
	// Ends are the run ends, in order, for consumers in memory.
	Ends []*agentturn.RunEnd `json:"-"`
	// Err is what went wrong: a run that could not start or ended in
	// error, a store failure, or a judge that could not reach a
	// verdict. A result with an error may still carry scores.
	Err error `json:"-"`
}

// MarshalJSON writes the result with Err as an "error" string.
func (r Result) MarshalJSON() ([]byte, error) {
	type plain Result
	aux := struct {
		plain
		Error string `json:"error,omitempty"`
	}{plain: plain(r)}
	if r.Err != nil {
		aux.Error = r.Err.Error()
	}
	if aux.Scores == nil {
		aux.Scores = []Score{}
	}
	return json.Marshal(aux)
}

// UnmarshalJSON reads a result written by MarshalJSON; an "error"
// string becomes Err.
func (r *Result) UnmarshalJSON(data []byte) error {
	type plain Result
	var aux struct {
		plain
		Error string `json:"error"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*r = Result(aux.plain)
	if aux.Error != "" {
		r.Err = errors.New(aux.Error)
	}
	return nil
}

// Run sends every task of the suite through the configuration and
// returns the report. A task whose run or judging failed has its error
// on its result; Run itself fails only when it cannot start, or when
// ctx is done before every task has run, in which case the report
// holds the results so far.
func (r *Runner) Run(ctx context.Context, suite *Suite) (*Report, error) {
	if r.Store == nil {
		return nil, errors.New("agenteval: runner has no store")
	}
	if r.Config == nil {
		return nil, errors.New("agenteval: runner has no config")
	}
	if suite == nil || len(suite.Tasks) == 0 {
		return nil, errors.New("agenteval: suite has no tasks")
	}
	report := &Report{Suite: suite.Name, Manifest: suite.Manifest, Results: make([]Result, len(suite.Tasks))}
	width := max(r.Parallel, 1)
	sem := make(chan struct{}, width)
	var wg sync.WaitGroup
	for i, task := range suite.Tasks {
		if ctx.Err() != nil {
			report.Results[i] = Result{Task: task, Err: ctx.Err()}
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			report.Results[i] = r.runTask(ctx, suite, task)
		}()
	}
	wg.Wait()
	report.ByJudge = Summarize(report.Results)
	return report, ctx.Err()
}

// runTask runs one task in a fresh session and judges it.
func (r *Runner) runTask(ctx context.Context, suite *Suite, task Task) Result {
	res := Result{Task: task}
	inputs := task.Inputs()
	if len(inputs) == 0 {
		res.Err = fmt.Errorf("agenteval: task %s has nothing to send", task.ID)
		return res
	}
	var header agentsession.Header
	if r.Header != nil {
		header = r.Header(task)
	}
	rec, s, err := session.Start(ctx, r.Store, header)
	if err != nil {
		res.Err = fmt.Errorf("agenteval: task %s: %w", task.ID, err)
		return res
	}
	res.SessionID = s.ID()
	if err := r.describe(ctx, s.ID(), suite, task); err != nil {
		res.Err = fmt.Errorf("agenteval: task %s: %w", task.ID, err)
		return res
	}
	a := agentturn.New(r.Config(task))
	unsubscribe := rec.Attach(a)
	for _, item := range inputs {
		end, err := a.Prompt(ctx, item)
		if end != nil {
			res.Ends = append(res.Ends, end)
			res.Runs++
			res.Reason = end.Reason
		}
		if err != nil {
			res.Err = fmt.Errorf("agenteval: task %s: run %d: %w", task.ID, res.Runs, err)
			break
		}
		if end.Reason != agentturn.ReasonDone && end.Reason != agentturn.ReasonStopped {
			// input_required and aborted leave calls pending, which the
			// next prompt cannot answer; the task ends here.
			break
		}
	}
	unsubscribe()
	res.Target = s.Leaf()
	res.Usage, res.CostUSD = r.usage(s, res.Target)
	if res.Target == "" {
		return res
	}
	t, err := trajectoryAt(s, res.Target)
	if err != nil {
		res.Err = errors.Join(res.Err, fmt.Errorf("agenteval: task %s: %w", task.ID, err))
		return res
	}
	for _, j := range r.Judges {
		score, err := j.Judge(ctx, t, task)
		if err != nil {
			res.Err = errors.Join(res.Err, fmt.Errorf("agenteval: task %s: judge %s: %w", task.ID, j.Name(), err))
			continue
		}
		if score.Judge == "" {
			score.Judge = j.Name()
		}
		entry, err := NewOutcome(score, res.Target, task.ID)
		if err != nil {
			res.Err = errors.Join(res.Err, fmt.Errorf("agenteval: task %s: judge %s: %w", task.ID, j.Name(), err))
			continue
		}
		if _, err := r.Store.Append(ctx, s.ID(), entry); err != nil {
			res.Err = errors.Join(res.Err, fmt.Errorf("agenteval: task %s: record %s: %w", task.ID, j.Name(), err))
			continue
		}
		res.Scores = append(res.Scores, score)
	}
	return res
}

// describe writes what the session is a run of: an info entry naming
// the task, an env entry whose read files are the suite manifest, and
// a custom entry carrying the suite, the task and its setup.
func (r *Runner) describe(ctx context.Context, sessionID string, suite *Suite, task Task) error {
	if _, err := r.Store.Append(ctx, sessionID, &agentsession.InfoEntry{Name: task.ID}); err != nil {
		return err
	}
	if len(suite.Manifest.Files) > 0 {
		env := &agentsession.EnvEntry{Files: &agentsession.FileHashes{Read: make(map[string]string, len(suite.Manifest.Files))}}
		for p, h := range suite.Manifest.Files {
			env.Files.Read[p] = h
		}
		if _, err := r.Store.Append(ctx, sessionID, env); err != nil {
			return err
		}
	}
	data, err := json.Marshal(TaskRecord{Suite: suite.Name, Location: suite.Manifest.Location, Task: task.ID, Setup: task.Setup, Meta: task.Meta})
	if err != nil {
		return err
	}
	_, err = r.Store.Append(ctx, sessionID, &agentsession.CustomEntry{NS: TaskNS, Data: data})
	return err
}

// usage sums the usage of the responses on the path to leaf and prices
// them when the runner can.
func (r *Runner) usage(s *agentsession.Session, leaf string) (openresponses.Usage, *float64) {
	var u openresponses.Usage
	cost, priced := 0.0, r.Cost != nil
	for _, e := range s.Path(leaf) {
		resp, ok := e.(*agentsession.ResponseEntry)
		if !ok || resp.Usage == nil {
			continue
		}
		u.InputTokens += resp.Usage.InputTokens
		u.OutputTokens += resp.Usage.OutputTokens
		u.TotalTokens += resp.Usage.TotalTokens
		u.InputTokensDetails.CachedTokens += resp.Usage.InputTokensDetails.CachedTokens
		u.OutputTokensDetails.ReasoningTokens += resp.Usage.OutputTokensDetails.ReasoningTokens
		if priced {
			usd, ok := r.Cost(resp.Model, *resp.Usage)
			priced = ok
			cost += usd
		}
	}
	if !priced {
		return u, nil
	}
	return u, &cost
}

// trajectoryAt returns the session's trajectory ending at leaf.
func trajectoryAt(s *agentsession.Session, leaf string) (export.Trajectory, error) {
	for t, err := range export.Trajectories(s) {
		if err != nil {
			return export.Trajectory{}, err
		}
		if t.LeafID == leaf {
			return t, nil
		}
	}
	return export.Trajectory{}, fmt.Errorf("agenteval: %w: no trajectory ends at %s", agentsession.ErrNoEntry, leaf)
}
