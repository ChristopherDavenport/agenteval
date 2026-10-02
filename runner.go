package agenteval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/ChristopherDavenport/agenteval/replay"
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
	// Config returns the configuration under test for a task.
	// Required unless ConfigWith or Build is set.
	Config func(Task) agentturn.Config
	// ConfigWith is Config with the recorder that writes the run's
	// session, and wins over Config when set. It is where a
	// configuration binds anything that must reach the record:
	// compact.WithOnFold(rec.Fold), without which a compacting
	// configuration records no fold and its session cannot be replayed
	// strictly, and the recorder itself, which a layer keeps to
	// annotate through Recorder.Annotate the run it is in.
	//
	//	ConfigWith: func(t agenteval.Task, rec *session.Recorder) agentturn.Config {
	//		cfg := product.Config(t)
	//		cfg.Transform = compact.NewLocal(cfg.Model, compact.WithOnFold(rec.Fold)).Transform
	//		return cfg
	//	}
	ConfigWith func(Task, *session.Recorder) agentturn.Config
	// Build is ConfigWith for a configuration whose construction can
	// fail or holds something to release, such as a kit that opens MCP
	// clients, and wins over both when set. Its error ends the task on
	// Result.Err; the close it returns, when not nil, is called once the
	// task is done, after its judges, even when the build failed, and
	// its error is joined there too.
	Build func(ctx context.Context, t Task, rec *session.Recorder) (cfg agentturn.Config, close func() error, err error)
	// SessionOptions, when set, supplies the options each run's
	// recorder is opened with: session.WithInstructionsParts for a
	// configuration that composes its instructions from parts, so the
	// session records which part changed rather than the whole prompt
	// on every config entry. The recorder is opened before the
	// configuration is built, so a function it is given here that needs
	// the configuration, such as one calling a kit's PartsFor, reaches
	// it through a closure over what Build builds a moment later.
	SessionOptions func(Task) []session.Option
	// Judges score each run once it has ended. Each score is appended
	// to the run's session as an outcome entry. A task whose run failed,
	// because the loop or Answer returned an error, is not judged unless
	// JudgeFailedRuns is set: an inference server that went away is not
	// the configuration's answer, and a score of 0 for it would count
	// in the report's mean and, for a product that takes scores as
	// rewards, as a reward.
	Judges []Judge
	// JudgeFailedRuns judges a task whose run failed as one that ended,
	// for an evaluation where a crash counts against the configuration.
	JudgeFailedRuns bool
	// Samples is how many times each task is run, each in a session of
	// its own: the group an RL producer scores together. Zero or one
	// means once. Each sample's Task, as every hook and judge sees it,
	// carries Meta["sample"], from "1", so a Header that fixes session
	// IDs can fix one per sample; a suite whose task sets that key
	// itself is refused.
	Samples int
	// GroupJudges score each task's samples together once all of them
	// are judged, on the goroutine of the last to finish. Each score is
	// appended to its sample's session as an outcome entry, like any
	// other. A group with a sample that was not judged, a run that
	// failed under the default of leaving it unjudged, is not group
	// judged, as an RL producer drops an incomplete group; every other
	// sample's result says so on Err.
	GroupJudges []GroupJudge
	// Answer answers the calls a run left pending when it ended
	// input_required, so an evaluation of a product whose policy asks
	// measures the whole run rather than the part before the first
	// ask. The run is resumed with what it returns and the resume is
	// recorded like any other turn; no answers, or a nil Answer, ends
	// the task there as before. agentpolicy.Engine.Answers satisfies
	// this signature as written.
	Answer func(context.Context, *agentturn.RunEnd) ([]agentturn.Answer, error)
	// MaxResumes bounds how many times one prompt may be resumed
	// through Answer, so an answer source that keeps a call pending
	// cannot loop. Zero means [DefaultMaxResumes].
	MaxResumes int
	// Parallel bounds how many tasks run at once; zero or one means one
	// at a time.
	Parallel int
	// Header, when set, supplies each run's session header: a harness
	// name, a working directory, a fixed ID for a test. Empty fields
	// are filled by the store. A header that names a Base and its
	// ParentSession forks that session, and the agent starts from the
	// context at the base, so a task runs as a continuation of a
	// recorded run and its session replays strictly like any other:
	// through a runner with the same Header under replay.AfterBase(),
	// since the agent starts after the base and never sends the
	// base's requests, or whole, from an agent that sends them.
	Header func(Task) agentsession.Header
	// Cost prices one model call, as export.Options.Cost does; see
	// price.Hook. When set, and every call of a run is priced, the
	// result carries the run's cost.
	Cost func(model string, usage openresponses.Usage) (float64, bool)
}

// Result is one task's run and its scores.
type Result struct {
	Task Task `json:"task"`
	// Sample is which run of the task this is, from 1, when
	// [Runner.Samples] is above one; 0 otherwise.
	Sample int `json:"sample,omitempty"`
	// SessionID is the session the run was recorded in.
	SessionID string `json:"session_id"`
	// Target is the entry every score of this result targets: the last
	// entry of the run, before any outcome was appended.
	Target string `json:"target,omitempty"`
	// Reason is how the last run of the task ended.
	Reason agentturn.Reason `json:"reason,omitempty"`
	// Runs is how many runs of the loop the task took: one per prompt
	// sent and one per resume.
	Runs int `json:"runs"`
	// Resumes is how many of those runs were resumes through
	// [Runner.Answer].
	Resumes int `json:"resumes,omitempty"`
	// ResumeBound says a prompt was still waiting on input when
	// [Runner.MaxResumes] stopped resuming it, where a task that ends
	// input_required without it stopped because there was no Answer,
	// or it had nothing to say or failed.
	ResumeBound bool `json:"resume_bound,omitempty"`
	// Usage is the sum over the model calls on the run's path: every
	// response, every fold or branch summary that reported usage, and
	// the summary calls of every fold that failed. It is the path the
	// exported document's final metrics sum, and the runner prices
	// each call under the model the exporter does, so with the same
	// price hook the two agree on the total whenever every call was
	// priced, except that the exporter as of agentsession v0.0.19
	// leaves a failed fold's calls out (agentsession#184).
	Usage openresponses.Usage `json:"usage"`
	// CostUSD is the run's cost under Runner.Cost, when every call was
	// priced.
	CostUSD *float64 `json:"cost_usd,omitempty"`
	// Scores are the judges' verdicts, in the runner's judge order. A
	// judge that failed is missing here and named in Err; a task whose
	// run failed has none, unless [Runner.JudgeFailedRuns].
	Scores []Score `json:"scores"`
	// Ends are the run ends, in order, for consumers in memory.
	Ends []*agentturn.RunEnd `json:"-"`
	// Err is what went wrong: a run that could not start or ended in
	// error, a store failure, or a judge that could not reach a
	// verdict. A result with an error may still carry scores.
	Err error `json:"-"`

	// trajectory is what the judges read, and judged says they did,
	// for the group step.
	trajectory export.Trajectory
	judged     bool
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
	if r.Config == nil && r.ConfigWith == nil && r.Build == nil {
		return nil, errors.New("agenteval: runner has no config")
	}
	if suite == nil || len(suite.Tasks) == 0 {
		return nil, errors.New("agenteval: suite has no tasks")
	}
	n := max(r.Samples, 1)
	if n > 1 {
		for _, task := range suite.Tasks {
			if _, ok := task.Meta[SampleMeta]; ok {
				return nil, fmt.Errorf("agenteval: task %s sets Meta[%q], which the runner sets on each sample", task.ID, SampleMeta)
			}
		}
	}
	report := &Report{Suite: suite.Name, Manifest: suite.Manifest, Results: make([]Result, len(suite.Tasks)*n)}
	// left counts each task's samples still running; the one that
	// takes it to zero runs the group step, and the atomic orders the
	// other samples' results before its reads.
	left := make([]atomic.Int32, len(suite.Tasks))
	width := max(r.Parallel, 1)
	sem := make(chan struct{}, width)
	var wg sync.WaitGroup
	for i, task := range suite.Tasks {
		left[i].Store(int32(n))
		group := report.Results[i*n : (i+1)*n]
		for k := range n {
			sample, run := 0, task
			if n > 1 {
				sample, run = k+1, withSample(task, k+1)
			}
			if ctx.Err() != nil {
				group[k] = Result{Task: run, Sample: sample, Err: ctx.Err()}
				continue
			}
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				group[k] = r.runTask(ctx, suite, run, sample)
				if left[i].Add(-1) == 0 {
					r.judgeGroup(ctx, task, group)
				}
			}()
		}
	}
	wg.Wait()
	report.ByJudge = Summarize(report.Results)
	return report, ctx.Err()
}

// runTask runs one task in a fresh session and judges it.
func (r *Runner) runTask(ctx context.Context, suite *Suite, task Task, sample int) (res Result) {
	res.Task, res.Sample = task, sample
	inputs := task.Inputs()
	if len(inputs) == 0 {
		res.Err = fmt.Errorf("agenteval: task %s has nothing to send", task.ID)
		return res
	}
	var header agentsession.Header
	if r.Header != nil {
		header = r.Header(task)
	}
	var opts []session.Option
	if r.SessionOptions != nil {
		opts = r.SessionOptions(task)
	}
	rec, s, err := session.Start(ctx, r.Store, header, opts...)
	if err != nil {
		res.Err = fmt.Errorf("agenteval: task %s: %w", task.ID, err)
		return res
	}
	res.SessionID = s.ID()
	// A fork starts from the context at its base, which Start seeded
	// the recorder with; an agent that did not start there too would
	// send requests the record does not describe, and the recorder
	// would hash none of them.
	var seed []agentturn.Option
	if s.Header().Base != "" {
		seed, err = session.AgentOptions(s, rec.ReadOptions()...)
		if err != nil {
			res.Err = fmt.Errorf("agenteval: task %s: base %s: %w", task.ID, s.Header().Base, err)
			return res
		}
	}
	if err := r.describe(ctx, s, suite, task, sample); err != nil {
		res.Err = fmt.Errorf("agenteval: task %s: %w", task.ID, err)
		return res
	}
	cfg, closeConfig, err := r.config(ctx, task, rec)
	if closeConfig != nil {
		defer func() {
			if err := closeConfig(); err != nil {
				res.Err = errors.Join(res.Err, fmt.Errorf("agenteval: task %s: close: %w", task.ID, err))
			}
		}()
	}
	if err != nil {
		res.Err = fmt.Errorf("agenteval: task %s: build: %w", task.ID, err)
		return res
	}
	a := agentturn.New(cfg, seed...)
	unsubscribe := rec.Attach(a)
	failed := false
	for _, item := range inputs {
		end, err := a.Prompt(ctx, item)
		if end != nil {
			res.Ends = append(res.Ends, end)
			res.Runs++
			res.Reason = end.Reason
		}
		if err == nil {
			end, err = r.answer(ctx, a, end, &res)
		}
		if err != nil {
			res.Err = fmt.Errorf("agenteval: task %s: run %d: %w", task.ID, res.Runs, err)
			failed = true
			break
		}
		if end.Reason != agentturn.ReasonDone && end.Reason != agentturn.ReasonStopped {
			// input_required with no answer, and aborted, leave calls
			// pending, which the next prompt cannot answer; the task
			// ends here.
			break
		}
	}
	unsubscribe()
	res.Target = s.Leaf()
	res.Usage, res.CostUSD = r.usage(s, res.Target)
	if res.Target == "" {
		return res
	}
	// replayable and trajectoryAt are this package's own, and their
	// errors already name it. Wrapping those with the package again
	// puts it twice in front of one message; the sites below wrap
	// another package's error, or one with no name of its own, and
	// name this one because nothing else would.
	if err := replayable(s, res.Target); err != nil {
		res.Err = errors.Join(res.Err, fmt.Errorf("task %s: %w", task.ID, err))
	}
	if failed && !r.JudgeFailedRuns {
		return res
	}
	t, err := trajectoryAt(s, res.Target)
	if err != nil {
		res.Err = errors.Join(res.Err, fmt.Errorf("task %s: %w", task.ID, err))
		return res
	}
	res.trajectory, res.judged = t, true
	for _, j := range r.Judges {
		score, err := j.Judge(ctx, t, task)
		if err != nil {
			res.Err = errors.Join(res.Err, fmt.Errorf("agenteval: task %s: judge %s: %w", task.ID, j.Name(), err))
			continue
		}
		if score.Judge == "" {
			score.Judge = j.Name()
		}
		entry, err := newOutcome(score, res.Target, task.ID, sample)
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

// config returns the configuration under test for a task and what
// releases it, from Build when it is set, then ConfigWith, then
// Config.
func (r *Runner) config(ctx context.Context, task Task, rec *session.Recorder) (agentturn.Config, func() error, error) {
	switch {
	case r.Build != nil:
		return r.Build(ctx, task, rec)
	case r.ConfigWith != nil:
		return r.ConfigWith(task, rec), nil, nil
	}
	return r.Config(task), nil, nil
}

// DefaultMaxResumes is how many times one prompt is resumed through
// [Runner.Answer] when [Runner.MaxResumes] is zero.
const DefaultMaxResumes = 8

// answer resumes a run that ended input_required with what
// Runner.Answer says, up to the bound, and returns the end it stopped
// at. Each resume is one more run on the result and is recorded like
// any other, so the trajectory a judge reads is the whole run.
func (r *Runner) answer(ctx context.Context, a *agentturn.Agent, end *agentturn.RunEnd, res *Result) (*agentturn.RunEnd, error) {
	if r.Answer == nil {
		return end, nil
	}
	bound := r.MaxResumes
	if bound <= 0 {
		bound = DefaultMaxResumes
	}
	for range bound {
		if end == nil || end.Reason != agentturn.ReasonInputRequired {
			return end, nil
		}
		answers, err := r.Answer(ctx, end)
		if err != nil {
			return end, fmt.Errorf("answer: %w", err)
		}
		if len(answers) == 0 {
			// The answer source had nothing to say about these calls,
			// which is a decision and not a failure.
			return end, nil
		}
		next, err := a.Resume(ctx, answers...)
		if next != nil {
			res.Ends = append(res.Ends, next)
			res.Runs++
			res.Resumes++
			res.Reason = next.Reason
			end = next
		}
		if err != nil {
			return end, fmt.Errorf("resume %d: %w", res.Resumes, err)
		}
	}
	if end != nil && end.Reason == agentturn.ReasonInputRequired {
		res.ResumeBound = true
	}
	return end, nil
}

// ErrUnreplayable is joined onto the result of a run whose session
// cannot be replayed strictly, because the record does not rebuild
// every request the run sent.
var ErrUnreplayable = errors.New("agenteval: the run cannot be replayed strictly")

// replayable reports whether the record rebuilds every request the run
// made in one environment. Whether it does is [replay.Unverifiable]'s
// question and is asked there rather than answered again here: a
// strict replay of a session it rejects cannot be built at all, and
// naming that on the result is the word missing at write time, weeks
// before anyone tries. What this adds is the runner's own diagnosis,
// which replay cannot make because it never sees the configuration: a
// session with unhashed responses and no fold at all is most often one
// whose compacting configuration never bound compact.WithOnFold, and
// that is a seam this package owns. A workspace substitution alone
// gets none of it; replay's error names the env entry. Nor does a
// first unhashed response whose agentturn:unhashed entry names a
// reasoning item as what the path rebuilds where the request parts
// from it: agentturn v0.0.15 leaves another model's reasoning out of a
// request, as a fork run under another model than its base's does, the
// format cannot describe that, and no seam here binds it; replay's
// error quotes the entry (#46). An unbound fold writes such an entry
// too, naming a message for a message, and keeps the diagnosis.
func replayable(s *agentsession.Session, leaf string) error {
	err := replay.Unverifiable(s, leaf)
	if err == nil {
		return nil
	}
	// Unverifiable also reports a leaf it could not resolve, which is a
	// different failure and gets none of the diagnosis below: a session
	// with no entries has no seam to have gone unbound.
	if !errors.Is(err, replay.ErrUnverifiable) {
		return err
	}
	if replay.Unverifiable(s, leaf, replay.AllowSubstitution()) == nil {
		return fmt.Errorf("%w: %w", ErrUnreplayable, err)
	}
	if leaf == "" {
		leaf = s.Leaf()
	}
	// why is the last agentturn:unhashed entry before the first
	// unhashed response, with no hashed response between.
	var why *agentsession.CustomEntry
	found := false
	for _, e := range s.Path(leaf) {
		switch v := e.(type) {
		case *agentsession.CompactionEntry:
			return fmt.Errorf("%w: %w", ErrUnreplayable, err)
		case *agentsession.CustomEntry:
			if v.NS == session.UnhashedNS && !found {
				why = v
			}
		case *agentsession.ResponseEntry:
			if !found && v.RequestHash == "" {
				found = true
			} else if !found {
				why = nil
			}
		}
	}
	if why != nil {
		var u session.Unhashed
		if json.Unmarshal(why.Data, &u) == nil && u.Recorded != nil && u.Recorded.Type == openresponses.ItemTypeReasoning {
			return fmt.Errorf("%w: %w", ErrUnreplayable, err)
		}
	}
	return fmt.Errorf("%w: %w and the session records no fold; a configuration that compacts binds compact.WithOnFold(rec.Fold) through Runner.ConfigWith, and a transform or a hook that edits the input is a change the record cannot describe at all", ErrUnreplayable, err)
}

// describe writes what the session is a run of: an info entry naming
// the task, an env entry whose read files are the suite manifest, and
// a custom entry carrying the suite, the task and its setup.
//
// The env entry restates the environment in force at the session's
// leaf, its workspace, directory, VCS state and tools, with the
// manifest as its files. A fork's leaf is its base's, and after the
// base's responses an env entry that named no workspace where the base
// ran in one would read as a substitution, which a strict replay
// refuses.
func (r *Runner) describe(ctx context.Context, s *agentsession.Session, suite *Suite, task Task, sample int) error {
	sessionID := s.ID()
	if _, err := r.Store.Append(ctx, sessionID, &agentsession.InfoEntry{Name: task.ID}); err != nil {
		return err
	}
	if len(suite.Manifest.Files) > 0 {
		env := &agentsession.EnvEntry{Files: &agentsession.FileHashes{Read: make(map[string]string, len(suite.Manifest.Files))}}
		if prev := lastEnv(s); prev != nil {
			env.CWD, env.VCS, env.Tools, env.Workspace = prev.CWD, prev.VCS, prev.Tools, prev.Workspace
		}
		for p, h := range suite.Manifest.Files {
			env.Files.Read[p] = h
		}
		if _, err := r.Store.Append(ctx, sessionID, env); err != nil {
			return err
		}
	}
	rec := TaskRecord{Suite: suite.Name, Location: suite.Manifest.Location, Task: task.ID, Setup: task.Setup, Meta: task.Meta, Sample: sample}
	if sample > 0 {
		rec.Samples = r.Samples
	}
	for _, j := range r.GroupJudges {
		rec.GroupJudges = append(rec.GroupJudges, j.Name())
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	_, err = r.Store.Append(ctx, sessionID, &agentsession.CustomEntry{NS: TaskNS, Data: data})
	return err
}

// SampleMeta is the Task.Meta key the runner sets on each sample of a
// task when [Runner.Samples] is above one, to the sample's number from
// "1".
const SampleMeta = "sample"

// withSample returns task as its sample'th run sees it: Meta copied,
// with SampleMeta set.
func withSample(task Task, sample int) Task {
	meta := make(map[string]string, len(task.Meta)+1)
	maps.Copy(meta, task.Meta)
	meta[SampleMeta] = strconv.Itoa(sample)
	task.Meta = meta
	return task
}

// judgeGroup runs the group judges over a task's samples, once each
// has run and been judged, and records each score on its sample.
func (r *Runner) judgeGroup(ctx context.Context, task Task, group []Result) {
	if len(r.GroupJudges) == 0 {
		return
	}
	var unjudged []string
	for _, res := range group {
		if !res.judged {
			unjudged = append(unjudged, strconv.Itoa(res.Sample))
		}
	}
	if len(unjudged) > 0 {
		err := fmt.Errorf("agenteval: task %s: group not judged: sample %s was not judged", task.ID, strings.Join(unjudged, ", "))
		for k := range group {
			if group[k].judged {
				group[k].Err = errors.Join(group[k].Err, err)
			}
		}
		return
	}
	members := make([]Member, len(group))
	for k, res := range group {
		members[k] = Member{Trajectory: res.trajectory, Scores: slices.Clone(res.Scores)}
	}
	for _, j := range r.GroupJudges {
		scores, err := j.JudgeGroup(ctx, members, task)
		if err == nil && len(scores) != len(group) {
			err = fmt.Errorf("%d scores for %d samples", len(scores), len(group))
		}
		if err != nil {
			for k := range group {
				group[k].Err = errors.Join(group[k].Err, fmt.Errorf("agenteval: task %s: group judge %s: %w", task.ID, j.Name(), err))
			}
			continue
		}
		for k, score := range scores {
			res := &group[k]
			if score.Judge == "" {
				score.Judge = j.Name()
			}
			entry, err := newOutcome(score, res.Target, task.ID, res.Sample)
			if err == nil {
				_, err = r.Store.Append(ctx, res.SessionID, entry)
			}
			if err != nil {
				res.Err = errors.Join(res.Err, fmt.Errorf("agenteval: task %s: group judge %s: %w", task.ID, j.Name(), err))
				continue
			}
			res.Scores = append(res.Scores, score)
		}
	}
}

// lastEnv returns the env entry in force at the session's leaf, or nil
// when the path holds none.
func lastEnv(s *agentsession.Session) *agentsession.EnvEntry {
	path := s.Path(s.Leaf())
	for i := len(path) - 1; i >= 0; i-- {
		if e, ok := path[i].(*agentsession.EnvEntry); ok {
			return e
		}
	}
	return nil
}

// usage sums the usage of the model calls on the path to leaf, the
// responses and the folds, failed or not, and prices them when the
// runner can.
func (r *Runner) usage(s *agentsession.Session, leaf string) (openresponses.Usage, *float64) {
	var u openresponses.Usage
	cost, priced := 0.0, r.Cost != nil
	// The model in force, for a fold or a response that does not name
	// its own, replayed as the exporter replays it so the two price
	// every call under the same model.
	model := ""
	for _, e := range s.Path(leaf) {
		var eu *openresponses.Usage
		m := model
		switch v := e.(type) {
		case *agentsession.ConfigEntry:
			if v.Replace {
				model = ""
			}
			if v.Model != "" {
				model = v.Model
			}
			continue
		case *agentsession.ResponseEntry:
			eu = v.Usage
			if v.Model != "" {
				m = v.Model
			}
		case *agentsession.CompactionEntry:
			model = v.Config.Model
			eu, m = v.Usage, v.Config.Model
		case *agentsession.BranchSummaryEntry:
			eu = v.Usage
		case *agentsession.CustomEntry:
			// A fold that failed still made its summary calls, and
			// paid for them; the entry holds their usage summed.
			// It leaves the model in force as it was.
			if v.NS != session.FailedFoldNS {
				continue
			}
			var f session.FailedFold
			if json.Unmarshal(v.Data, &f) != nil {
				continue
			}
			eu = f.Usage
			if f.Model != "" {
				m = f.Model
			}
		default:
			continue
		}
		if eu == nil {
			continue
		}
		u.InputTokens += eu.InputTokens
		u.OutputTokens += eu.OutputTokens
		u.TotalTokens += eu.TotalTokens
		u.InputTokensDetails.CachedTokens += eu.InputTokensDetails.CachedTokens
		u.OutputTokensDetails.ReasoningTokens += eu.OutputTokensDetails.ReasoningTokens
		if priced {
			usd, ok := r.Cost(m, *eu)
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
