# Plan: evaluation layer

What makes the record useful rather than merely complete. The session
library records every request, response, tool output and config delta,
and exports ATIF trajectories; nothing consumes them. This module
does: a model that replays recorded responses so hooks, tools and
fronts are tested against real traffic offline; a runner that sends a
task, or a recorded session's prompts, through a configuration and
records the result; a judge contract; and scores written back into the
session as `outcome` entries in the shape the format defines for them.

The reference shapes are Harbor, Terminal-Bench's harness and the
owner of the ATIF format, mini-SWE-agent as the smallest agent worth
benchmarking, and Inspect AI's log for what a report should hold. The
study under `../examples/harbor-eval` built a minimal agent on the
core, ran it against those references, and its findings are folded
into this plan; `../feedback.md` holds them in issue form.

## Goals

- `replay.Model`: an `openresponses.Streamer` that serves a session's
  recorded responses in path order and, in strict mode, refuses to
  serve a request whose hash differs from the recorded one. A fold by
  a compaction transform is served from the compaction entry that
  recorded it, so a compacted run replays too.
- `replay.Tools`: recorded tool outputs served by call, so a whole run
  can be replayed without touching the world.
- A task and suite shape, loaded from an `fs.FS` with a manifest.
- A runner that executes a task under a `Config`, records a fresh
  session in a store, and returns the trajectory.
- A judge contract, deterministic judges, and a judge that is itself
  an agent with a rubric.
- Scores as `outcome` entries on the session, and a report over a
  suite.
- A comparison: the same tasks under two configurations, judged the
  same way, with the difference between the configurations named.
- Harbor's verifier reward read back as scores, and a price table so
  a run can be costed.

## Non-goals

- Datasets and benchmarks. Suites are the product's or the study's;
  this module loads them.
- Sandboxes and environments. Harbor owns the container; the runner
  runs where it is started. A Harbor task loaded here is its prompt
  and metadata, not its environment or verifier.
- A leaderboard, a dashboard or a database of results. The report is
  a value and a JSON file.
- Statistical machinery beyond mean, count and pass rate. A product
  that wants confidence intervals computes them from the report.
- Capping spend. That needs a check before the model call, which is
  an `agentturn` change (`CostLimit` beside `MaxTurns`); the price
  table here is what such a check would read.

## Module and packages

Separate module, `github.com/ChristopherDavenport/agenteval`.

```
agenteval/                   Task, Suite, Manifest, Judge, Score, Runner, Result, Report, Compare
agenteval/replay             Model and Tools served from a recorded session
agenteval/judge              The deterministic judges and the rubric judge
agenteval/price              A price table and the cost hook the export takes
agenteval/harbor             Nested module: a Harbor task as a Task, a verifier reward as scores
```

The root module depends on `openresponses`, `agenttool`, `agentturn`,
its `session` nested module, `agentsession` and the standard library.
It is the only sibling that imports both the loop and the session
library, because it runs one to produce the other. `harbor` needs a
TOML decoder, which that rule forbids, so it is a nested module.

The judge contract lives in the root package rather than in `judge`:
a judge takes a `Task`, the runner takes judges, and two packages
that each import the other cannot build. `judge` holds the
implementations.

## Core types

### `replay`

```go
// Model serves a session's recorded model calls in path order: each
// response entry on the path from the root to the leaf is one call,
// and each compaction entry is the fold that preceded the call after
// it. CreateStream serves the next response; Compact serves the next
// compaction, so a configuration folding through compact.New replays
// through the same model. A fold made through compact.NewLocal is an
// ordinary call whose answer the session holds only as the summary
// item, and it is served from the compaction entry when that entry is
// next on the path.
func NewModel(s *agentsession.Session, opts ...Option) (*Model, error)

// Strict makes the model compare the hash of each request it receives
// with the hash recorded on the response entry it is about to serve.
// A mismatch returns ErrDiverged naming the entry and both hashes. The
// default is lenient: responses are served by position and the hashes
// are reported through the observer. A local fold is checked the same
// way against the hash its compaction entry's fold member records; a
// fold through the compaction endpoint, and one recorded before that
// member existed, is checked for the shape of a fold's request only.
func Strict() Option
// BeforeModelCall is an agentturn Config.BeforeModelCall that replaces
// the request's instructions and tool list with the ones in force at
// the recorded call about to be served, rebuilt from the config
// entries on the path. A product whose layers re-read state each turn
// chains it last, and Settings holds every step's settings for a
// reader.
func (m *Model) BeforeModelCall(ctx context.Context, req *openresponses.Request) error
func (m *Model) Settings() []agentsession.Settings
func (m *Model) SettingsAt(n int) (agentsession.Settings, bool)
// WithLeaf names the path. A session that was branched has several
// leaves, and its current leaf, after judging, is an outcome entry
// rather than a model output, so a replay of a judged session names
// the leaf it wants.
func WithLeaf(id string) Option
func WithObserver(fn func(Served)) Option
// WithFoldText says how a local fold's summary item becomes the text
// the model answered with; the default undoes compact.SummaryMessage.
func WithFoldText(fn func(summary openresponses.Item) string) Option

// Tools wraps a tool list so that a call whose call_id, or whose name
// and arguments, match a recorded function_call output returns that
// output instead of running. Arguments are compared canonically (RFC
// 8785), the same rule the request hash uses. An unmatched call falls
// through to the real tool, or fails when Strict.
func Tools(s *agentsession.Session, tools []agenttool.Tool, opts ...Option) []agenttool.Tool
```

The hash compared is `agentsession.RequestHash(session.Canonical(req))`:
`Canonical` in `agentturn/session` is the one place that says which
request members are transport and which reached the model, and a
replay that hashed the request as received would fail every
comparison on `stream`.

Strict mode is the fidelity test: a hook, a transform or a front that
changes what the model would have been sent fails loudly against real
traffic. What the model was sent is not the model and the tools alone,
which is why the settings come out of `NewModel` too: the composition
study's product diverged at its first call because a layer rebuilt the
instructions from a store the recorded run itself had written to, and
replayed strictly only once the instructions in force at each recorded
response were served back to it. Lenient mode is the fixture: a TUI or a session recorder is
exercised by a real run with no model behind it.

### Tasks and suites

```go
type Task struct {
    ID          string
    Instruction string              // the common case: one user message
    Prompts     openresponses.Items // the multi-turn case; wins when set
    Setup       map[string]string   // product-defined, such as a repo revision
    Expect      json.RawMessage     // judge-defined
    Meta        map[string]string
}

type Manifest struct {
    Location string            // where in the fs.FS the suite was loaded from
    Files    map[string]string // path -> "sha256:..." of every file loaded
}

type Suite struct {
    Name     string
    Tasks    []Task
    Manifest Manifest
}

func LoadSuite(fsys fs.FS, location string) (*Suite, error)  // one task per JSON file
func FromSession(s *agentsession.Session) (Task, error)      // the user prompts on the current path
```

`Instruction` exists because a Harbor instruction is one Markdown
string and a hand-written task file should not have to spell out an
Open Responses message to say so. Each prompt is one run of the loop,
in order; the runner sends the next when the previous ended `done` or
`stopped`.

`FromSession` is the comparison's input: a recorded run becomes a task
whose prompts are what the user said, so the same conversation can be
sent through a different configuration.

### Judge contract

```go
type Score struct {
    Judge   string          // the judge's name; the outcome's label
    Value   float64         // on the judge's own scale; comparable, not normalised
    Pass    bool
    Reason  string
    Details json.RawMessage // the judge's own record
    Session string          // the judge's own session, when it recorded one
}

type Judge interface {
    Name() string
    Judge(ctx context.Context, t export.Trajectory, task Task) (Score, error)
}
```

`Value` is unbounded because Harbor's verifier reward is
(`dict[str, float | int]` with only a finiteness check) and the format's
`score` is any finite number. The deterministic judges use 0 and 1; a
judge that normalises keeps the raw number in `Details`.

### Runner

```go
type Runner struct {
    Store    agentsession.Store                      // where each run's session is created
    Config   func(Task) agentturn.Config             // the configuration under test, per task
    // ConfigWith is Config with the recorder that writes the run, and
    // wins over Config. It is where a compacting configuration binds
    // compact.WithOnFold(rec.Fold), without which the folds reach no
    // session and the run cannot be replayed strictly, and where a
    // layer keeps the recorder to Annotate the run it is in.
    ConfigWith func(Task, *session.Recorder) agentturn.Config
    // Answer answers the calls a run left pending when it ended
    // input_required; the run is resumed with them, up to MaxResumes
    // times, and every resume is recorded. agentpolicy.Engine.Answers
    // satisfies it as written.
    Answer     func(context.Context, *agentturn.RunEnd) ([]agentturn.Answer, error)
    MaxResumes int
    Judges   []Judge
    Parallel int
    Header   func(Task) agentsession.Header          // optional: harness, cwd, a fixed ID
    Cost     func(model string, usage openresponses.Usage) (float64, bool) // optional: price.Hook
}

type Result struct {
    Task      Task
    SessionID string
    Target    string             // the entry every score of this result targets
    Reason    agentturn.Reason   // how the last run ended
    Runs      int                // one per prompt sent and one per resume
    Resumes   int                // how many of them were resumes through Answer
    Usage     openresponses.Usage
    CostUSD   *float64           // when Cost is set and priced every call
    Scores    []Score
    Ends      []*agentturn.RunEnd // in memory only
    Err       error
}

func (r *Runner) Run(ctx context.Context, suite *Suite) (*Report, error)
```

Each task runs in a fresh session recorded through `agentturn/session`.
The recorder is made before the configuration, so `ConfigWith` can hand
it to the configuration under test; a run whose session ends with a
response carrying no request hash is reported on the result as
`ErrUnreplayable`, because that is exactly the session a strict replay
will refuse and nothing else says so at write time.
Before the run the session holds an `info` entry naming the task, an
`env` entry whose `files.read` is the suite manifest, and a `custom`
entry in the `agenteval:task` namespace carrying the suite name, its
location, the task ID and the task's setup and metadata. The exporter
lifts the `env` entry to the document's `extra.environment`, so a
trajectory a judge reads names the suite version it came from.

Each score is appended as an `outcome` entry in the shape RFC 0001
draft 0.2 defines:

- `kind` is `agentsession.OutcomeEval`, the kind the RFC defines for a
  score produced by an evaluation run.
- `target` is the last entry of the run judged, which is what a reader
  that selects branches by outcome resolves. A task ID is on no path
  and would count for nothing.
- `score` is `Value`, `pass` is `Pass` and `label` is `Judge`, each in
  the entry's own field.
- `details` has a fixed schema, `OutcomeDetails`: `task`, `reason`,
  `session` (the judge's own session, for an agent judge) and `judge`
  (the judge's own details). A pass rate is computable from the
  session files without knowing any judge's private JSON.

The mapping needs the code above; nothing about branch preference
comes free. `export.PreferScore` takes each branch's best score across
judges, while the report averages a judge's scores across tasks, so
the two can name different winners; the report documents its rule.

The rubric judge's session names the judged session as its
`parent_session`, so a store lists a run's judgements by filter, and
the outcome's `details.session` points the other way. The format's
`link` relations are a closed set with nothing meaning "judged by", so
no link entry is written.

### `judge`

```go
func Exact(field string) Judge           // final assistant text equals Expect, or Expect[field]
func Contains(field string) Judge        // final assistant text contains every string in Expect, or Expect[field]
func ToolCalled(name string) Judge       // a call to name appears in the context at the leaf
func Rubric(cfg agentturn.Config, rubric string, opts ...Option) Judge

func WithStore(store agentsession.Store) Option  // record the judge's own session
func WithName(name string) Option
func WithRender(fn func(export.Trajectory) (string, error)) Option
func StripRaw() export.Redactor                  // drops extra.openresponses from a document
```

The rubric judge is an agent configuration: the trajectory is rendered
as its input, the rubric is its instructions, and its output is
constrained to a score schema through `Text.Format`. The rendering is
the ATIF document with the raw item passthrough stripped, because the
study measured that passthrough at half a document and a judge's
context is better spent once. It runs under the same loop and records
its own session when given a store, so a judgement is as replayable as
the run it judged.

### Report and comparison

```go
type Summary struct { Count int; Mean float64; Passed int; PassRate float64 }

type Report struct {
    Suite    string
    Manifest Manifest
    Results  []Result
    ByJudge  map[string]Summary // mean and pass rate over the tasks the judge scored
}

func (r *Report) WriteJSON(w io.Writer) error

// Compare runs the suite under two configurations and pairs results by
// task, with each judge's difference and the difference between the
// settings the two runs started under.
func Compare(ctx context.Context, suite *Suite, a, b *Runner) (*Comparison, error)

type Comparison struct {
    Suite    string
    Manifest Manifest
    A, B     *Report
    Config   ConfigDiff       // when every pair's differs the same way
    Uniform  bool
    Pairs    []Pair           // Task, A, B, Delta by judge, Config
    ByJudge  map[string]float64 // B's mean minus A's
}

func DiffSettings(a, b agentsession.Settings) ConfigDiff
```

A comparison of two configurations writes two sessions per task, so
`PreferScore` plays no part in it; putting both on branches of one
session needs `agentturn/session` to grow a `ResumeAt` first (study
finding 6) and is deferred.

### `price`

```go
type Rates struct { Input, Cached, Output float64 } // USD per million tokens
type Table map[string]Rates                          // by model name; "provider/model" and "model" both looked up
func Load(fsys fs.FS, path string) (Table, error)
func (t Table) For(model string) (Rates, bool)
func (t Table) Cost(model string, u openresponses.Usage) (float64, bool)
func Hook(t Table) func(model string, usage openresponses.Usage) (float64, bool) // export.Options.Cost
```

The cost formula is ATIF's: `(prompt - cached) x input + cached x cached
+ completion x output`. The runner and the exporter take the same hook,
so the report and the exported document agree on one number. Prices
belong with the thing that compares runs, not in the wire package or
the record.

### `harbor`

```go
// Reward reads a finished trial's verifier output: reward.json, an
// object of labelled numbers, each a Score named by its key; else
// reward.txt, one number, a Score named "reward".
func Reward(fsys fs.FS, dir string) ([]agenteval.Score, error)

// Load reads a Harbor task directory: instruction.md as Instruction,
// [task].name as ID, and task.toml copied into Setup and Meta. It is
// the prompt only; running the task faithfully means running it under
// Harbor.
func Load(fsys fs.FS, dir string) (agenteval.Task, error)
```

The reward direction is the one that pays: a labelled numeric metric
is exactly a `Score` and exactly what an `outcome` entry holds. The
task direction carries the instruction and a few keys and loses the
Dockerfile, the network policy, the resource requests, the oracle
solution, the steps and the verifier; `Load` says so and honours none
of what it copies.

## Conventions shared with the siblings

1. One decision vocabulary: the loop's. This module makes no decisions
   about calls; it observes runs.
2. Sources through `fs.FS` with a manifest. A suite is one, and its
   manifest is recorded in every run's `env` entry, so a result names
   the suite version it came from.
3. Recording through entries the session format already has: scores
   are `outcome` entries, runs are ordinary sessions, and the judge's
   own run is a session whose header names its parent. Nothing is
   added to the RFC.

## Invariants

- A strict replay of an unmodified session never diverges, folds
  included.
- A run's session, exported, contains everything the report says
  about it; the report holds no fact the session lacks. Cost is the
  one derived number, and it names its source: the runner's price
  hook, which the export takes too.
- Scores are appended, never rewritten; a second judgement is a
  second `outcome` entry.
- The runner never mutates a suite's `fs.FS`.

## Testing

`replay` against sessions recorded from the `echo` adapter: strict
passes, a changed instruction diverges at the right entry, a compacted
run replays, `Tools` serves by ID and by arguments. The runner over a
two-task suite with the deterministic judges, asserting the `outcome`
entries and the report. The rubric judge against the echo adapter with
a canned schema answer. A golden report under `testdata/report/`,
written through a store that assigns fixed IDs and times so the file
is stable. `price` against a table fixture; `harbor` against a task
directory and a trial directory.

## Milestones

1. `replay.NewModel` lenient and strict, with the observer and folds.
2. `replay.Tools`.
3. `Task`, `Suite`, `LoadSuite`, `FromSession`, the manifest.
4. The judge contract and the three deterministic judges.
5. `Runner`, `outcome` entries, `Report`.
6. `judge.Rubric`, `Compare`.
7. `price` and `harbor`.
8. The Harbor study's agent, run inside Harbor, every run exported and
   judged, in its own repository.

## Open questions

- Whether `Compare` should also offer the two configurations as two
  branches of one session, so `PreferScore` has forks to choose
  between. Needs `session.ResumeAt`; deferred until it exists.
- Where a judgement of a human-labelled trajectory goes: the same
  `outcome` entry with `kind: "feedback"` and `label: "human"` is the
  obvious answer, and the export's preference already reads it.

## Resolved

- The fold is checked like any other call. `agentturn/session` v0.0.6
  records `FoldCall.RequestHash` on every compaction entry a local
  fold produces, so strict mode hashes the fold's own request and
  refuses a summary prompt, a budget or a filter that changed. The
  shape test, no tools and no instructions, decides only whether the
  request is a fold at all, and is the whole check for a fold through
  the compaction endpoint, which sends no request the format hashes.
- `Tools` matches arguments canonically: `agentsession` already has
  RFC 8785 in its request hash, the same bytes decide both, and byte
  comparison makes a replay fail when a serialiser reorders a map.
- `Compare` diffs the two configurations: both sessions' settings at
  the first call replay from their config entries, and the difference
  in model, instructions, reasoning, text, tools and passthrough
  members is the description of what was compared.
- The Harbor adapter is a nested module here, `harbor`, and the
  reward direction was built first.
