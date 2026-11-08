package agenteval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"

	"github.com/ChristopherDavenport/agentsession"
	"github.com/ChristopherDavenport/agentturn"
	"github.com/ChristopherDavenport/agentturn/session"
	"github.com/ChristopherDavenport/openresponses"
)

// Comparison is the same suite run under two configurations and judged
// the same way, paired by task.
type Comparison struct {
	Suite    string   `json:"suite"`
	Manifest Manifest `json:"manifest"`
	// A and B are the two reports. They are not written by WriteJSON;
	// the pairs carry both results of every task.
	A, B *Report `json:"-"`
	// Config is the difference between the settings B's runs started
	// under and A's, when every pair differs the same way; Uniform
	// says whether they did. Each pair carries its own. Whether a run
	// folded depends on the task as much as the configuration, so
	// pairs are uniform whatever their Folded and FoldFailed say, and
	// Config.Folded says whether any of A's runs folded and any of
	// B's, when those differ; Config.FoldFailed the same for a fold
	// that failed.
	Config  ConfigDiff `json:"config"`
	Uniform bool       `json:"uniform"`
	Pairs   []Pair     `json:"pairs"`
	// ByJudge is B's mean minus A's, per judge, over the pairs the judge
	// scored on both sides. A pair with an unjudged side, a run that
	// failed under the default of leaving it unjudged, is left out of
	// it and counted on Unjudged: the mean says how the two compared
	// where both answered, and Unjudged how often one did not. An
	// evaluation where a crash counts as a failure, Harbor's included,
	// sets [Runner.JudgeFailedRuns] on both runners, and the failed run
	// then scores 0 here like any other.
	ByJudge map[string]float64 `json:"by_judge"`
	// Unjudged is how many pairs have a side that was not judged; each
	// such pair says which on [Pair.Unjudged].
	Unjudged int `json:"unjudged"`
}

// The values of [Pair.Unjudged]: which side's result was not judged.
const (
	UnjudgedA    = "a"
	UnjudgedB    = "b"
	UnjudgedBoth = "both"
)

// Pair is one task under both configurations.
type Pair struct {
	Task string `json:"task"`
	// Sample is the sample of the task both results are, when the
	// runners sampled; the runs are paired by task and sample.
	Sample int     `json:"sample,omitempty"`
	A      *Result `json:"a"`
	B      *Result `json:"b"`
	// Delta is B's value minus A's, per judge that scored both.
	Delta map[string]float64 `json:"delta"`
	// Unjudged is [UnjudgedA], [UnjudgedB] or [UnjudgedBoth] when that
	// side's result was not judged: its run failed and
	// [Runner.JudgeFailedRuns] was off, or it never reached a
	// trajectory. The side has no scores, so Delta holds nothing for
	// the judges, and the pair is counted on [Comparison.Unjudged]
	// rather than in its ByJudge. Empty when both sides were judged.
	Unjudged string `json:"unjudged,omitempty"`
	// Config is the difference between the settings B's run started
	// under and A's.
	Config ConfigDiff `json:"config"`
}

// Change is one setting under A and under B.
type Change struct {
	A any `json:"a"`
	B any `json:"b"`
}

// ConfigDiff is what differed between two runs' settings: the settings
// each run's first model call was made under, replayed from its
// session's config entries.
type ConfigDiff struct {
	Model        *Change `json:"model,omitempty"`
	Instructions *Change `json:"instructions,omitempty"`
	Reasoning    *Change `json:"reasoning,omitempty"`
	Text         *Change `json:"text,omitempty"`
	// ToolsAdded names tools B has and A lacks, ToolsRemoved the
	// reverse, and ToolsChanged tools both have under a different
	// definition; each sorted.
	ToolsAdded   []string `json:"tools_added,omitempty"`
	ToolsRemoved []string `json:"tools_removed,omitempty"`
	ToolsChanged []string `json:"tools_changed,omitempty"`
	// Extra holds passthrough members that differ, by wire name; a
	// side that lacks the member has nil.
	Extra map[string]Change `json:"extra,omitempty"`
	// BeyondSettings says the two runs' first calls sent different
	// requests although their settings were the same: a transform, a
	// BeforeTurn or BeforeModelCall hook, or items one side injected.
	// None of those is a setting, so no field above can name it, and a
	// comparison of two configurations that differ only there would
	// otherwise report that nothing differed, which is worse than
	// reporting nothing. It is read from the request hashes the two
	// first responses recorded: two that differ say so, and one hash
	// against none says the other run sent something the record does
	// not rebuild, which is a transform or a hook by definition. It
	// compares the first call of each run, so a transform that
	// changes nothing until later, as a compaction transform does not
	// fold before it has anything to fold, is not visible here;
	// Folded is.
	BeyondSettings bool `json:"beyond_settings,omitempty"`
	// Folded says one run's path held a compaction entry and the
	// other's none, with whether each side folded: a context strategy
	// is not a setting either, and the fold is what it leaves on the
	// path. Two runs that both folded, however often, do not differ
	// here. On a [Comparison] it is over all the runs of each side.
	Folded *Change `json:"folded,omitempty"`
	// FoldFailed is Folded for a fold that failed: one run's path held
	// an agentturn:compaction_failed entry and the other's none. A
	// configuration whose every fold fails, on a summary model too
	// verbose for agentturn to accept its summary, leaves no compaction
	// entry, and without this would compare as one that never tried to
	// compact, though each failed fold paid for its summary calls.
	FoldFailed *Change `json:"fold_failed,omitempty"`
}

// Empty reports whether nothing differed.
func (d ConfigDiff) Empty() bool {
	return !d.BeyondSettings && d.Folded == nil && d.FoldFailed == nil && d.Model == nil && d.Instructions == nil && d.Reasoning == nil && d.Text == nil &&
		len(d.ToolsAdded) == 0 && len(d.ToolsRemoved) == 0 && len(d.ToolsChanged) == 0 && len(d.Extra) == 0
}

// Compare runs the suite under a and b and pairs the results by task.
// The runners share nothing but the suite; each records its own
// sessions in its own store, which may be the same store. The
// comparison's Config is the difference between the settings the two
// runs of each task started under, read back from their sessions, so
// a comparison says how the configurations differed and not only which
// scored higher.
//
// A pair whose side was not judged, because its run failed and that
// runner leaves a failed run unjudged, is marked on [Pair.Unjudged] and
// counted on [Comparison.Unjudged]; its ByJudge covers the pairs both
// sides answered. A configuration that crashes on the hard tasks and
// answers the easy ones therefore does not read as the equal of one
// that answers all of them. For an evaluation where a crash counts as
// a failure, Harbor's included, which counts a trial with no reward as
// 0, set [Runner.JudgeFailedRuns] on both runners.
func Compare(ctx context.Context, suite *Suite, a, b *Runner) (*Comparison, error) {
	if a == nil || b == nil {
		return nil, errors.New("agenteval: compare needs two runners")
	}
	ra, err := a.Run(ctx, suite)
	if err != nil {
		return nil, fmt.Errorf("agenteval: compare: a: %w", err)
	}
	rb, err := b.Run(ctx, suite)
	if err != nil {
		return nil, fmt.Errorf("agenteval: compare: b: %w", err)
	}
	return compareResults(ctx, ra, rb, func(*Result) agentsession.Store { return a.Store }, func(*Result) agentsession.Store { return b.Store }), nil
}

// compareResults pairs two reports' results by task and sample, the
// settings each run started under read from its session in the store
// storeA or storeB gives for it.
func compareResults(ctx context.Context, ra, rb *Report, storeA, storeB func(*Result) agentsession.Store) *Comparison {
	c := &Comparison{Suite: ra.Suite, Manifest: ra.Manifest, A: ra, B: rb, Uniform: true, ByJudge: map[string]float64{}}
	counts := map[string]int{}
	var foldedA, foldedB, failedA, failedB bool
	for i := range ra.Results {
		pa := &ra.Results[i]
		pb, ok := rb.Sample(pa.Task.ID, pa.Sample)
		if !ok {
			continue
		}
		pair := Pair{Task: pa.Task.ID, Sample: pa.Sample, A: pa, B: pb, Delta: map[string]float64{}}
		switch {
		case !pa.judged && !pb.judged:
			pair.Unjudged = UnjudgedBoth
		case !pa.judged:
			pair.Unjudged = UnjudgedA
		case !pb.judged:
			pair.Unjudged = UnjudgedB
		}
		if pair.Unjudged != "" {
			c.Unjudged++
		}
		byJudge := map[string]float64{}
		for _, s := range pa.Scores {
			byJudge[s.Judge] = s.Value
		}
		for _, s := range pb.Scores {
			if va, ok := byJudge[s.Judge]; ok {
				d := s.Value - va
				pair.Delta[s.Judge] = d
				c.ByJudge[s.Judge] += d
				counts[s.Judge]++
			}
		}
		sa, ha, fa, errA := initialCall(ctx, storeA(pa), pa)
		sb, hb, fb, errB := initialCall(ctx, storeB(pb), pb)
		if errA == nil && errB == nil {
			pair.Config = DiffSettings(sa, sb)
			// The same settings and a different request is something
			// the settings cannot describe: a transform, a hook or an
			// injected item.
			if pair.Config.Empty() && differentFirstCall(ha, hb) {
				pair.Config.BeyondSettings = true
			}
			if fa.folded != fb.folded {
				pair.Config.Folded = &Change{A: fa.folded, B: fb.folded}
			}
			if fa.failed != fb.failed {
				pair.Config.FoldFailed = &Change{A: fa.failed, B: fb.failed}
			}
			foldedA, foldedB = foldedA || fa.folded, foldedB || fb.folded
			failedA, failedB = failedA || fa.failed, failedB || fb.failed
		}
		settings := pair.Config
		settings.Folded, settings.FoldFailed = nil, nil
		if i == 0 {
			c.Config = settings
		} else if !reflect.DeepEqual(c.Config, settings) {
			c.Uniform = false
		}
		c.Pairs = append(c.Pairs, pair)
	}
	for name, sum := range c.ByJudge {
		c.ByJudge[name] = sum / float64(counts[name])
	}
	if !c.Uniform {
		c.Config = ConfigDiff{}
	}
	if foldedA != foldedB {
		c.Config.Folded = &Change{A: foldedA, B: foldedB}
	}
	if failedA != failedB {
		c.Config.FoldFailed = &Change{A: failedA, B: failedB}
	}
	return c
}

// ReadOption configures how [ResultAt] and [CompareReports] read a
// stored run back.
type ReadOption func(*readOptions)

type readOptions struct {
	cost func(string, openresponses.Usage) (float64, bool)
}

// WithCost prices each model call of a stored run, as [Runner.Cost]
// prices a run, so the result carries its cost.
func WithCost(fn func(model string, usage openresponses.Usage) (float64, bool)) ReadOption {
	return func(o *readOptions) { o.cost = fn }
}

// ResultAt reads a run back from its session in store as a result,
// with no model and no judge called: an evaluation baseline a ref pins
// (RFC 0002, Refs), a run another process made, the results a report
// file names. target is the entry the run's scores target, as
// [Result.Target] is; "" takes the target of the last outcome on the
// path to the session's leaf, else its last run end. The task and
// sample are the path's last agenteval:task record, the scores the
// outcomes targeting target that the runner wrote for that task and
// sample (RFC 0001, outcome), kind eval or a verifier's test, in the
// order they were appended, less a harness's score for a run that
// failed with no verdict ([OutcomeDetails].Error), which is on Err and
// leaves the run unjudged; Snapshot the last [SnapshotNS] record of
// the run at target for that task and sample, one whose data does not
// hash to its digest kept as recorded with the mismatch on its Error,
// not on Err; the
// reason the last run end on the path to target, Runs and Resumes the
// runs there, and Usage, ModelCalls, CostUSD and Subagents are read as
// the runner reads them, so a stored result and the one Run returned
// agree. The result counts as judged when it holds a score, or when
// its run ended other than in error, as the runner judges it under the
// default; Ends and Err are not read back. The session is read without
// a hold where the store can.
func ResultAt(ctx context.Context, store agentsession.Store, sessionID, target string, opts ...ReadOption) (*Result, error) {
	var o readOptions
	for _, opt := range opts {
		opt(&o)
	}
	s, err := readSession(ctx, store, sessionID)
	if err != nil {
		return nil, fmt.Errorf("agenteval: session %s: %w", sessionID, err)
	}
	if target == "" {
		target = storedTarget(s)
	}
	path := s.Path(target)
	if path == nil {
		return nil, fmt.Errorf("agenteval: session %s: %w: %s", sessionID, agentsession.ErrNoEntry, target)
	}
	res := &Result{SessionID: s.ID(), Target: target}
	rec, hasRecord := lastTaskRecord(path)
	if hasRecord {
		res.Task = Task{ID: rec.Task, Setup: rec.Setup, Meta: rec.Meta}
		if rec.Sample > 0 {
			res.Sample = rec.Sample
			res.Task = withSample(res.Task, rec.Sample)
		}
	}
	ended, errored := false, false
	for _, run := range agentsession.Runs(path) {
		res.Runs++
		if run.Start.Source == agentsession.SourceResume {
			res.Resumes++
		}
		if run.End != nil {
			res.Reason, ended = agentturn.Reason(run.End.Reason), true
		}
	}
	for _, e := range s.Entries() {
		out, ok := e.(*agentsession.OutcomeEntry)
		if !ok || out.Target != target {
			continue
		}
		score, d, ok := ReadOutcome(out)
		if !ok || hasRecord && (d.Task != rec.Task || d.Sample != rec.Sample) {
			continue
		}
		if d.Error != "" {
			// A harness's score for a run that failed with no verdict:
			// the run is unjudged, and the error says why.
			errored = true
			res.Err = errors.Join(res.Err, fmt.Errorf("%s: %s", score.Judge, d.Error))
			continue
		}
		res.Scores = append(res.Scores, score)
	}
	res.judged = len(res.Scores) > 0 || !errored && ended && res.Reason != agentturn.ReasonError
	res.Snapshot = snapshotAt(s, target, rec.Task, rec.Sample, hasRecord)
	m := measure(ctx, store, o.cost, s, target)
	res.Usage, res.ModelCalls, res.CostUSD, res.Subagents = m.usage, m.calls, m.cost, m.delegated
	return res, nil
}

// storedTarget is the entry a stored run's scores target: the target of
// the last outcome on the path to the leaf, else the last run end
// there, else the leaf.
func storedTarget(s *agentsession.Session) string {
	path := s.Path(s.Leaf())
	for i := len(path) - 1; i >= 0; i-- {
		if out, ok := path[i].(*agentsession.OutcomeEntry); ok && (out.Kind == agentsession.OutcomeEval || out.Kind == agentsession.OutcomeTest) {
			return out.Target
		}
	}
	for i := len(path) - 1; i >= 0; i-- {
		if run, ok := path[i].(*agentsession.RunEntry); ok && run.IsEnd() {
			return run.ID
		}
	}
	return s.Leaf()
}

// lastTaskRecord returns the last task record on a path: a fork's path
// holds its base's too.
func lastTaskRecord(path []agentsession.Entry) (TaskRecord, bool) {
	for i := len(path) - 1; i >= 0; i-- {
		c, ok := path[i].(*agentsession.CustomEntry)
		if !ok || c.NS != TaskNS {
			continue
		}
		var rec TaskRecord
		if err := json.Unmarshal(c.Data, &rec); err != nil {
			return TaskRecord{}, false
		}
		return rec, true
	}
	return TaskRecord{}, false
}

// CompareReports compares two sets of stored results, paired by task
// and sample as [Compare] pairs them, without running anything: a
// candidate against a baseline a ref pins, two reports read from
// files, a Harbor job imported against another. A report holds no fact
// its sessions lack, so each result is read again from its store, the
// one resolveA gives for a's result and resolveB for b's: [OneStore]
// for a report whose sessions share a store, as a runner's do, and
// harbor.Resolver for an imported job, whose trials each have one. Each
// is read at the session and target the result names,
// with [ResultAt]: the scores, the run's end and the settings compared
// are the record's, not the report's. A session with no task record
// takes the task and sample its report names. A result whose session cannot be
// read fails the comparison. The suite and manifest are a's.
func CompareReports(ctx context.Context, a, b *Report, resolveA, resolveB func(Result) (agentsession.Store, error), opts ...ReadOption) (*Comparison, error) {
	if a == nil || b == nil || resolveA == nil || resolveB == nil {
		return nil, errors.New("agenteval: compare reports needs two reports and their stores")
	}
	ra, storesA, err := storedReport(ctx, a, resolveA, opts)
	if err != nil {
		return nil, fmt.Errorf("agenteval: compare: a: %w", err)
	}
	rb, storesB, err := storedReport(ctx, b, resolveB, opts)
	if err != nil {
		return nil, fmt.Errorf("agenteval: compare: b: %w", err)
	}
	return compareResults(ctx, ra, rb, storesA.of, storesB.of), nil
}

// OneStore is the resolver of a report whose every session is in
// store, as a runner's are, for [CompareReports].
func OneStore(store agentsession.Store) func(Result) (agentsession.Store, error) {
	return func(Result) (agentsession.Store, error) {
		if store == nil {
			return nil, errors.New("no store")
		}
		return store, nil
	}
}

// storesOf remembers the store each result was read from, by session.
type storesOf map[string]agentsession.Store

func (s storesOf) of(r *Result) agentsession.Store { return s[r.SessionID] }

// storedReport is r with each result read again from the store resolve
// gives for it, and the store of each.
func storedReport(ctx context.Context, r *Report, resolve func(Result) (agentsession.Store, error), opts []ReadOption) (*Report, storesOf, error) {
	out := &Report{Suite: r.Suite, Manifest: r.Manifest, Results: make([]Result, len(r.Results))}
	stores := storesOf{}
	for i, was := range r.Results {
		if was.SessionID == "" {
			return nil, nil, fmt.Errorf("task %s: no session", was.Task.ID)
		}
		store, err := resolve(was)
		if err != nil {
			return nil, nil, fmt.Errorf("task %s: store: %w", was.Task.ID, err)
		}
		res, err := ResultAt(ctx, store, was.SessionID, was.Target, opts...)
		if err != nil {
			return nil, nil, fmt.Errorf("task %s: %w", was.Task.ID, err)
		}
		stores[res.SessionID] = store
		// The task as the report names it, the record holding only its
		// ID, setup and metadata; and the report's task and sample for a
		// session that holds no record, as no session another harness
		// wrote does, Harbor's among them.
		switch res.Task.ID {
		case was.Task.ID:
			res.Task = was.Task
		case "":
			res.Task, res.Sample = was.Task, was.Sample
		}
		out.Results[i] = *res
	}
	out.ByJudge = Summarize(out.Results)
	return out, stores, nil
}

// initialCall replays the settings the result's first model call was
// made under, the config entries on the path to the first response
// entry, and returns the hash that response recorded for its request
// and whether the path to the result's target holds a compaction
// entry and a failed fold. A session with no response yields the
// settings at its leaf and no hash.
func initialCall(ctx context.Context, store agentsession.Store, res *Result) (agentsession.Settings, string, folds, error) {
	if res.SessionID == "" {
		return agentsession.Settings{}, "", folds{}, errors.New("agenteval: no session")
	}
	s, err := readSession(ctx, store, res.SessionID)
	if err != nil {
		return agentsession.Settings{}, "", folds{}, err
	}
	leaf := res.Target
	if leaf == "" {
		leaf = s.Leaf()
	}
	var folded folds
	for _, e := range s.Path(leaf) {
		switch v := e.(type) {
		case *agentsession.CompactionEntry:
			folded.folded = true
		case *agentsession.CustomEntry:
			if v.NS == session.FailedFoldNS {
				folded.failed = true
			}
		}
	}
	for _, e := range s.Path(leaf) {
		if resp, ok := e.(*agentsession.ResponseEntry); ok {
			cx, err := s.RequestContext(resp.ID)
			return cx.Settings, resp.RequestHash, folded, err
		}
	}
	cx, err := s.Context()
	return cx.Settings, "", folded, err
}

// folds is what a run's path holds of compaction: a fold applied, and
// a fold that failed.
type folds struct{ folded, failed bool }

// differentFirstCall reports whether two runs' first calls sent
// different requests, from the hashes their responses recorded. Two
// hashes that differ say so. One hash and none says the other run
// sent something the record does not rebuild, which is a transform, a
// hook or an injected item by definition. Two runs that both sent
// something the record cannot rebuild cannot be told apart this way.
func differentFirstCall(a, b string) bool {
	if a == "" && b == "" {
		return false
	}
	return a != b
}

// DiffSettings returns what differs between a and b. It covers the
// settings the record describes, which is the model, the instructions,
// the reasoning and text configuration, the tools and the passthrough
// members, and nothing else: a transform, a hook and an injected item
// are not settings. [Compare] reports one of those through
// [ConfigDiff.BeyondSettings].
func DiffSettings(a, b agentsession.Settings) ConfigDiff {
	var d ConfigDiff
	if a.Model != b.Model {
		d.Model = &Change{A: a.Model, B: b.Model}
	}
	if a.Instructions != b.Instructions {
		d.Instructions = &Change{A: a.Instructions, B: b.Instructions}
	}
	if !sameJSON(a.Reasoning, b.Reasoning) {
		d.Reasoning = &Change{A: a.Reasoning, B: b.Reasoning}
	}
	if !sameJSON(a.Text, b.Text) {
		d.Text = &Change{A: a.Text, B: b.Text}
	}
	d.ToolsAdded, d.ToolsRemoved, d.ToolsChanged = diffTools(a.Tools, b.Tools)
	for k, va := range a.Extra {
		vb, ok := b.Extra[k]
		if !ok {
			d.extra(k, Change{A: rawValue(va)})
		} else if !sameJSON(va, vb) {
			d.extra(k, Change{A: rawValue(va), B: rawValue(vb)})
		}
	}
	for k, vb := range b.Extra {
		if _, ok := a.Extra[k]; !ok {
			d.extra(k, Change{B: rawValue(vb)})
		}
	}
	return d
}

func (d *ConfigDiff) extra(k string, c Change) {
	if d.Extra == nil {
		d.Extra = map[string]Change{}
	}
	d.Extra[k] = c
}

// diffTools compares two tool lists by name.
func diffTools(a, b openresponses.Tools) (added, removed, changed []string) {
	byName := map[string]openresponses.Tool{}
	for _, t := range a {
		byName[agentsession.ToolName(t)] = t
	}
	seen := map[string]bool{}
	for _, t := range b {
		name := agentsession.ToolName(t)
		seen[name] = true
		old, ok := byName[name]
		switch {
		case !ok:
			added = append(added, name)
		case !sameJSON(old, t):
			changed = append(changed, name)
		}
	}
	for _, t := range a {
		if name := agentsession.ToolName(t); !seen[name] {
			removed = append(removed, name)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	sort.Strings(changed)
	return added, removed, changed
}

// rawValue decodes a passthrough member for the diff, so the report
// shows the value rather than its bytes.
func rawValue(raw json.RawMessage) any {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	return v
}

func sameJSON(a, b any) bool {
	da, errA := json.Marshal(a)
	db, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(da) == string(db)
}

// WriteJSON writes the comparison as indented JSON with a trailing
// newline. The two reports are not included; write them separately.
func (c *Comparison) WriteJSON(w io.Writer) error {
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	_, err = w.Write(append(data, '\n'))
	return err
}
