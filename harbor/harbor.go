// Package harbor reads what Harbor, the harness behind Terminal-Bench,
// writes and expects, into agenteval's shapes.
//
// The direction that carries the score is [Reward]: a Harbor verifier
// writes one number to reward.txt or an object of labelled numbers to
// reward.json, and a labelled number is exactly an agenteval.Score.
//
// [Load] goes the other way and carries less: a Harbor task is an
// instruction, a task.toml, an environment, an oracle solution and a
// verifier, and only the instruction and the table survive into a
// Task. The Dockerfile, the network policy, the resource requests, the
// MCP servers, the steps and the test script do not. A task loaded
// this way is the prompt only; running it faithfully means running it
// under Harbor, and Load honours nothing it copies into Setup.
//
// The two halves meet in a judge. A Harbor task's notion of success is
// a script someone else runs after the agent stops, so the judge that
// fits is not an expectation compared against the trajectory but a
// wrapper around Reward over the trial's verifier directory:
//
//	func verifier(fsys fs.FS, dir string) agenteval.Judge {
//		return agenteval.JudgeFunc{
//			JudgeName: "harbor",
//			Fn: func(context.Context, export.Trajectory, agenteval.Task) (agenteval.Score, error) {
//				scores, err := harbor.Reward(fsys, dir)
//				if err != nil {
//					return agenteval.Score{}, err
//				}
//				return scores[0], nil
//			},
//		}
//	}
//
// A run judged that way records Harbor's own number as an outcome
// entry on its session, and a report over the suite is a pass rate
// Harbor would recognise.
package harbor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/ChristopherDavenport/agenteval"
	"github.com/ChristopherDavenport/agentsession"
)

// File names Harbor uses.
const (
	InstructionFile = "instruction.md"
	TaskFile        = "task.toml"
	RewardJSON      = "reward.json"
	RewardText      = "reward.txt"
)

// DefaultLabel names the score read from reward.txt, which carries no
// label of its own. It is also the key Harbor reports a trial's reward
// under.
const DefaultLabel = "reward"

// ErrNoReward is returned by Reward when the directory holds neither
// reward file.
var ErrNoReward = errors.New("harbor: no reward file")

// Option configures [Reward].
type Option func(*rewardOptions)

type rewardOptions struct {
	pass func(label string, value float64) bool
}

// WithPass sets how a reward's value becomes a score's Pass. The
// default passes a value of at least 1, the 0-or-1 convention of
// Harbor's own test scripts; the format itself allows any finite
// number.
func WithPass(fn func(label string, value float64) bool) Option {
	return func(o *rewardOptions) { o.pass = fn }
}

// Reward reads a verifier's output from dir in fsys, which is
// /logs/verifier inside the container and the trial's verifier
// directory on the host: reward.json, an object of labelled numbers,
// each becoming a Score named by its key; else reward.txt, one number,
// a Score named [DefaultLabel]. The JSON wins when both exist, as it
// does for Harbor. Values are the verifier's own and unbounded; each
// score's Details records which file it came from.
func Reward(fsys fs.FS, dir string, opts ...Option) ([]agenteval.Score, error) {
	o := rewardOptions{pass: func(_ string, v float64) bool { return v >= 1 }}
	for _, opt := range opts {
		opt(&o)
	}
	if data, err := fs.ReadFile(fsys, path.Join(dir, RewardJSON)); err == nil {
		return fromJSON(data, o)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("harbor: read %s: %w", path.Join(dir, RewardJSON), err)
	}
	data, err := fs.ReadFile(fsys, path.Join(dir, RewardText))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w in %s", ErrNoReward, dir)
		}
		return nil, fmt.Errorf("harbor: read %s: %w", path.Join(dir, RewardText), err)
	}
	return fromText(data, o)
}

func fromJSON(data []byte, o rewardOptions) ([]agenteval.Score, error) {
	var rewards map[string]json.RawMessage
	if err := json.Unmarshal(data, &rewards); err != nil {
		return nil, fmt.Errorf("harbor: %s: %w", RewardJSON, err)
	}
	if len(rewards) == 0 {
		return nil, fmt.Errorf("harbor: %s holds no rewards", RewardJSON)
	}
	labels := make([]string, 0, len(rewards))
	for label := range rewards {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	var out []agenteval.Score
	for _, label := range labels {
		// The decoder has already refused anything that is not JSON, so
		// what is left to check is that the member is a number and not
		// a string that holds one.
		raw := string(rewards[label])
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil || strings.HasPrefix(raw, "\"") || math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, fmt.Errorf("harbor: %s: %s is not a finite number: %s", RewardJSON, label, raw)
		}
		out = append(out, score(label, v, RewardJSON, o))
	}
	return out, nil
}

func fromText(data []byte, o rewardOptions) ([]agenteval.Score, error) {
	text := strings.TrimSpace(string(data))
	v, err := strconv.ParseFloat(text, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return nil, fmt.Errorf("harbor: %s is not a finite number: %q", RewardText, text)
	}
	return []agenteval.Score{score(DefaultLabel, v, RewardText, o)}, nil
}

func score(label string, v float64, file string, o rewardOptions) agenteval.Score {
	details, _ := json.Marshal(map[string]any{"file": file, "value": v})
	return agenteval.Score{
		Judge:   label,
		Value:   v,
		Pass:    o.pass(label, v),
		Reason:  fmt.Sprintf("the verifier wrote %s to %s", strconv.FormatFloat(v, 'g', -1, 64), file),
		Details: details,
	}
}

// Load reads a Harbor task directory into a Task: instruction.md as
// Instruction, task.toml's [task].name as ID, [task] and [metadata]
// into Meta under the keys task.* and metadata.*, and every other
// table of task.toml into Setup under its own name, keys flattened
// with dots and values rendered as strings, lists and tables of
// tables as JSON. A missing task.toml names the task after the
// directory. What Setup holds is recorded with the run and honoured
// by nothing here.
//
// A runner that builds the task's environment itself reads these keys
// of Setup, as Harbor 0.25's TaskConfig defines them (the harbor
// package's models/task/config.py, which this does not restate):
//
//   - the environment: environment.docker_image, else the task's
//     environment/Dockerfile; environment.cpus, environment.memory_mb
//     and environment.storage_mb, or the sizes environment.memory and
//     environment.storage ("2G") that Terminal-Bench 2.0 tasks write
//     and Harbor converts; environment.gpus; environment.network_mode,
//     one of no-network, public (the default) and allowlist, with
//     environment.allowed_hosts for the last; environment.env,
//     environment.workdir, environment.build_timeout_sec and
//     environment.mcp_servers, a JSON list of {name, transport,
//     command or url}.
//   - the agent: agent.timeout_sec, agent.user, and agent.network_mode
//     and agent.allowed_hosts overriding the environment's.
//   - the verifier: verifier.timeout_sec, verifier.env and
//     verifier.environment_mode, shared or separate.
//
// Harbor ignores a key it does not define, and harbor lint reports it:
// environment.dockerfile is not one, nor is a network_mode of none.
//
// Harbor validates [task].name as org/name, so the ID of every real
// Harbor task holds a slash: a product that writes one file per task
// ID wants to know that before it tries.
//
// Both tables are prefixed because [task] is a package description
// with name, version, authors and keywords, while [metadata] is a
// free-form dict[str, Any] that invites the same words: unprefixed,
// a task carrying either key in both tables lost one of the two, and
// which one it lost depended on Go's map iteration order.
func Load(fsys fs.FS, dir string) (agenteval.Task, error) {
	instruction, err := fs.ReadFile(fsys, path.Join(dir, InstructionFile))
	if err != nil {
		return agenteval.Task{}, fmt.Errorf("harbor: read %s: %w", path.Join(dir, InstructionFile), err)
	}
	task := agenteval.Task{ID: path.Base(dir), Instruction: strings.TrimRight(string(instruction), "\n")}
	if task.Instruction == "" {
		return agenteval.Task{}, fmt.Errorf("harbor: %s is empty", path.Join(dir, InstructionFile))
	}
	data, err := fs.ReadFile(fsys, path.Join(dir, TaskFile))
	if errors.Is(err, fs.ErrNotExist) {
		return task, nil
	}
	if err != nil {
		return agenteval.Task{}, fmt.Errorf("harbor: read %s: %w", path.Join(dir, TaskFile), err)
	}
	var doc map[string]any
	if err := toml.Unmarshal(data, &doc); err != nil {
		return agenteval.Task{}, fmt.Errorf("harbor: decode %s: %w", path.Join(dir, TaskFile), err)
	}
	task.Meta = map[string]string{}
	task.Setup = map[string]string{}
	for key, value := range doc {
		switch key {
		case "task":
			if t, ok := value.(map[string]any); ok {
				if name, _ := t["name"].(string); name != "" {
					task.ID = name
				}
				flatten(task.Meta, "task", t)
			}
		case "metadata":
			if m, ok := value.(map[string]any); ok {
				flatten(task.Meta, "metadata", m)
			}
		default:
			flatten(task.Setup, key, value)
		}
	}
	if len(task.Meta) == 0 {
		task.Meta = nil
	}
	if len(task.Setup) == 0 {
		task.Setup = nil
	}
	return task, nil
}

// flatten renders a decoded TOML value into out under prefix: a table
// recurses with dotted keys, a scalar is formatted, and a list or an
// array of tables is JSON.
func flatten(out map[string]string, prefix string, value any) {
	key := func(k string) string {
		if prefix == "" {
			return k
		}
		return prefix + "." + k
	}
	switch v := value.(type) {
	case map[string]any:
		for k, sub := range v {
			flatten(out, key(k), sub)
		}
	case string:
		out[prefix] = v
	case bool:
		out[prefix] = strconv.FormatBool(v)
	case int64:
		out[prefix] = strconv.FormatInt(v, 10)
	case float64:
		out[prefix] = strconv.FormatFloat(v, 'g', -1, 64)
	default:
		data, err := json.Marshal(v)
		if err != nil {
			out[prefix] = fmt.Sprint(v)
			return
		}
		out[prefix] = string(data)
	}
}

// LabelPrefix precedes a reward's key in the label of the outcome
// [ImportJob] writes, so a verifier's reward is told from an agenteval
// judge's score of the same name.
const LabelPrefix = "harbor/"

// TrialMeta is the Task.Meta key [ImportJob] names a result's trial
// directory under.
const TrialMeta = "harbor.trial"

// ResultFile is the result.json Harbor writes for a job and for each of
// its trials.
const ResultFile = "result.json"

// trialResult is what ImportJob reads of a trial's result.json, as
// Harbor 0.25's TrialResult writes it.
type trialResult struct {
	TaskName       string `json:"task_name"`
	TrialName      string `json:"trial_name"`
	VerifierResult *struct {
		Rewards map[string]json.Number `json:"rewards"`
	} `json:"verifier_result"`
	ExceptionInfo *struct {
		Type    string `json:"exception_type"`
		Message string `json:"exception_message"`
	} `json:"exception_info"`
}

// ImportJob reads a finished Harbor job back: each trial's
// verifier_result.rewards is written onto the agent's session as
// outcome entries, and the job is returned as a Report, so Harbor runs
// get agenteval's reports and comparisons.
//
// job is the job directory, which holds one directory per trial with
// its result.json. open returns the store holding a trial's sessions,
// given the trial's directory name: the store the agent recorded into
// under the trial's agent directory, such as a jsonl store over
// agent/sessions. In it the trial's session is the one top-level
// session, one that names no parent session: a subagent's and a fork's
// are not the run, and a store holding more than one top-level session
// is an error on that trial. ImportJob does not close the stores.
//
// Each reward becomes an outcome of kind test, the kind RFC 0001 gives
// a verifier's score, labelled [LabelPrefix] and the reward's key, with
// its value as the score and [WithPass]'s rule as its pass, targeting
// the run's last entry, the last on the session's path that is not an
// outcome. Its details are agenteval.OutcomeDetails naming the trial's
// task. A trial that errored with no rewards, which Harbor counts as 0,
// is recorded as that 0 under [DefaultLabel], with no pass, which RFC
// 0001 reads as a verdict, and the exception in the details' error, and is reported unjudged, with the exception on the
// result's Err, so the 0 is not read as the verifier's verdict; one
// that errored after its verifier wrote rewards keeps the rewards and
// carries the exception on Err. An outcome the session already holds,
// the same kind, target, label, score and pass, is not written again,
// so importing a job twice writes nothing the second time.
//
// The report's results, in trial order, are each session read back
// with agenteval.ResultAt at the target, its Task named by the trial's
// task_name and its Meta carrying the trial's directory under
// [TrialMeta]. A task the job ran more than once, Harbor's n_attempts,
// is sampled: its trials are its samples, numbered from 1 in trial
// order on Result.Sample and Meta["sample"], as the runner numbers a
// task's samples, so two imported jobs pair attempt with attempt.
// A trial whose session cannot be found or written is reported with
// the error on Err. ImportJob fails only when the job cannot be read.
func ImportJob(ctx context.Context, job fs.FS, open func(trial string) (agentsession.Store, error), opts ...Option) (*agenteval.Report, error) {
	o := rewardOptions{pass: func(_ string, v float64) bool { return v >= 1 }}
	for _, opt := range opts {
		opt(&o)
	}
	dirs, err := fs.ReadDir(job, ".")
	if err != nil {
		return nil, fmt.Errorf("harbor: read job: %w", err)
	}
	report := &agenteval.Report{Suite: "harbor"}
	if data, err := fs.ReadFile(job, ResultFile); err == nil {
		var j struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(data, &j) == nil && j.ID != "" {
			report.Suite = "harbor/" + j.ID
		}
	}
	type trial struct {
		dir    string
		result trialResult
	}
	var trials []trial
	attempts := map[string]int{}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		data, err := fs.ReadFile(job, path.Join(d.Name(), ResultFile))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("harbor: read %s: %w", path.Join(d.Name(), ResultFile), err)
		}
		var tr trialResult
		if err := json.Unmarshal(data, &tr); err != nil {
			return nil, fmt.Errorf("harbor: %s: %w", path.Join(d.Name(), ResultFile), err)
		}
		if tr.TaskName == "" {
			tr.TaskName = d.Name()
		}
		trials = append(trials, trial{d.Name(), tr})
		attempts[tr.TaskName]++
	}
	// A task run more than once, Harbor's n_attempts, is sampled: its
	// trials are its samples, numbered from 1 in trial order.
	seen := map[string]int{}
	for _, t := range trials {
		sample := 0
		if attempts[t.result.TaskName] > 1 {
			seen[t.result.TaskName]++
			sample = seen[t.result.TaskName]
		}
		report.Results = append(report.Results, importTrial(ctx, t.dir, t.result, sample, open, o))
	}
	report.ByJudge = agenteval.Summarize(report.Results)
	return report, nil
}

// importTrial writes one trial's rewards onto its session and reads the
// session back as a result.
func importTrial(ctx context.Context, dir string, tr trialResult, sample int, open func(string) (agentsession.Store, error), o rewardOptions) agenteval.Result {
	task := agenteval.Task{ID: tr.TaskName, Meta: map[string]string{TrialMeta: dir}}
	if sample > 0 {
		task.Meta[agenteval.SampleMeta] = strconv.Itoa(sample)
	}
	res := agenteval.Result{Task: task, Sample: sample}
	fail := func(err error) agenteval.Result {
		res.Err = errors.Join(res.Err, fmt.Errorf("harbor: trial %s: %w", dir, err))
		return res
	}
	var exception string
	if e := tr.ExceptionInfo; e != nil {
		exception = strings.TrimSuffix(e.Type+": "+e.Message, ": ")
		res.Err = fmt.Errorf("harbor: trial %s: %s", dir, exception)
	}
	store, err := open(dir)
	if err != nil {
		return fail(err)
	}
	s, err := topLevel(ctx, store)
	if err != nil {
		return fail(err)
	}
	target := runTarget(s)
	if target == "" {
		return fail(fmt.Errorf("session %s holds no entry", s.ID()))
	}
	entries, err := rewardOutcomes(tr, exception, task.ID, target, o)
	if err != nil {
		return fail(err)
	}
	for _, e := range entries {
		if holds(s, e) {
			continue
		}
		if _, err := store.Append(ctx, s.ID(), e); err != nil {
			return fail(err)
		}
	}
	read, err := agenteval.ResultAt(ctx, store, s.ID(), target)
	if err != nil {
		return fail(err)
	}
	read.Task, read.Sample = task, sample
	read.Err = errors.Join(res.Err, read.Err)
	if exception != "" {
		// ResultAt says the same of an errored trial's 0; once is enough.
		read.Err = res.Err
	}
	return *read
}

// rewardOutcomes builds the outcomes a trial's rewards are written as.
func rewardOutcomes(tr trialResult, exception, task, target string, o rewardOptions) ([]*agentsession.OutcomeEntry, error) {
	var rewards map[string]json.Number
	if tr.VerifierResult != nil {
		rewards = tr.VerifierResult.Rewards
	}
	if len(rewards) == 0 {
		if exception == "" {
			return nil, errors.New("no rewards and no exception")
		}
		// Harbor counts an errored trial with no reward as 0.
		e, err := outcome(DefaultLabel, 0, nil, agenteval.OutcomeDetails{Task: task, Reason: "the trial errored and its verifier wrote no reward, which Harbor counts as 0", Error: exception}, target)
		if err != nil {
			return nil, err
		}
		return []*agentsession.OutcomeEntry{e}, nil
	}
	labels := make([]string, 0, len(rewards))
	for label := range rewards {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	var out []*agentsession.OutcomeEntry
	for _, label := range labels {
		v, err := rewards[label].Float64()
		if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, fmt.Errorf("reward %s is not a finite number: %s", label, rewards[label])
		}
		reason := fmt.Sprintf("the verifier rewarded %s", strconv.FormatFloat(v, 'g', -1, 64))
		pass := o.pass(label, v)
		e, err := outcome(label, v, &pass, agenteval.OutcomeDetails{Task: task, Reason: reason}, target)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

// outcome builds one reward's outcome entry, with no pass when pass is
// nil.
func outcome(label string, v float64, pass *bool, d agenteval.OutcomeDetails, target string) (*agentsession.OutcomeEntry, error) {
	details, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	e := agentsession.NewOutcomeEntry(agentsession.OutcomeTest, target).WithScore(v).WithLabel(LabelPrefix + label)
	if pass != nil {
		e = e.WithPass(*pass)
	}
	e.Details = details
	return e, nil
}

// holds reports whether s already holds an outcome equal to e.
func holds(s *agentsession.Session, e *agentsession.OutcomeEntry) bool {
	for _, x := range s.Entries() {
		o, ok := x.(*agentsession.OutcomeEntry)
		if !ok || o.Kind != e.Kind || o.Target != e.Target || o.Label != e.Label {
			continue
		}
		samePass := o.Pass == nil && e.Pass == nil || o.Pass != nil && e.Pass != nil && *o.Pass == *e.Pass
		if o.Score != nil && *o.Score == *e.Score && samePass {
			return true
		}
	}
	return false
}

// topLevel returns the one session of store that names no parent.
func topLevel(ctx context.Context, store agentsession.Store) (*agentsession.Session, error) {
	var found []string
	for sum, err := range store.List(ctx, agentsession.ListFilter{}) {
		if err != nil {
			return nil, fmt.Errorf("list sessions: %w", err)
		}
		if sum.Header.ParentSession == "" {
			found = append(found, sum.Header.ID)
		}
	}
	switch len(found) {
	case 0:
		return nil, errors.New("no top-level session in the trial's store")
	case 1:
	default:
		return nil, fmt.Errorf("%d top-level sessions in the trial's store: %s", len(found), strings.Join(found, ", "))
	}
	if rd, ok := store.(agentsession.Reader); ok {
		return rd.Read(ctx, found[0])
	}
	return store.Open(ctx, found[0])
}

// runTarget is the run's last entry: the last on the path to the
// session's leaf that is not an outcome, so a second import finds the
// target the first wrote to.
func runTarget(s *agentsession.Session) string {
	path := s.Path(s.Leaf())
	for i := len(path) - 1; i >= 0; i-- {
		if _, ok := path[i].(*agentsession.OutcomeEntry); !ok {
			return path[i].Base().ID
		}
	}
	return ""
}

// Resolver is the store resolver of a Report [ImportJob] returned, for
// agenteval.CompareReports: each result's store is the one open gives
// for its trial, which the result names under [TrialMeta], so the open
// the import was given serves the comparison too.
func Resolver(open func(trial string) (agentsession.Store, error)) func(agenteval.Result) (agentsession.Store, error) {
	return func(r agenteval.Result) (agentsession.Store, error) {
		trial := r.Task.Meta[TrialMeta]
		if trial == "" {
			return nil, fmt.Errorf("harbor: task %s names no trial under %s", r.Task.ID, TrialMeta)
		}
		return open(trial)
	}
}
