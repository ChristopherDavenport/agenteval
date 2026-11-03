# agenteval

Evaluation for Go agents over Open Responses: a replay model and
recorded tools served from a session, tasks and suites loaded from an
`fs.FS`, a runner that records every run as a session, judges, and
scores written back as `outcome` entries.

```sh
go get github.com/ChristopherDavenport/agenteval
```

## What it does

A run is an ordinary [agentsession](https://github.com/ChristopherDavenport/agentsession):
the runner sends a task through an [agentturn](https://github.com/ChristopherDavenport/agentturn)
configuration with the session recorder attached, so every request,
response and tool output is on disk and the exporter renders it as an
ATIF trajectory. Judges read that trajectory. Each score is appended
to the session as an `outcome` entry whose `target` is the run's last
entry, whose `kind` is `eval`, and whose `pass` and `score` are where
the format puts them, so a pass rate is computable from the session
files alone. The report is a value over those sessions and holds no
fact they lack.

```go
suite, _ := agenteval.LoadSuite(os.DirFS("evals"), "smoke")
r := &agenteval.Runner{
	Store:  store,
	Config: func(agenteval.Task) agentturn.Config { return cfg },
	Judges: []agenteval.Judge{judge.Contains("text"), judge.ToolCalled("bash")},
	Cost:   price.Hook(prices),
}
report, _ := r.Run(ctx, suite)
report.WriteJSON(os.Stdout)
```

A configuration that needs the run's recorder takes `ConfigWith`
instead: a compacting configuration binds `compact.WithOnFold` there,
without which its folds reach no session and the run cannot be
replayed, and a layer that annotates the run it is in keeps the same
recorder. A configuration whose construction can fail or holds
something to release, such as an agentkit kit, takes `Build`, which
returns an error and a close; `SessionOptions` opens each run's
recorder with options such as `session.WithInstructionsParts`, so the
session records which instructions part changed. A `Header` that names
a `Base` forks that session, and the agent starts from the context
there. `Answer` answers the calls a run left pending when it ended
`input_required` and resumes it, so a product whose policy asks is
measured over the whole run rather than the part before its first ask.

```go
r := &agenteval.Runner{
	Store: store,
	ConfigWith: func(t agenteval.Task, rec *session.Recorder) agentturn.Config {
		cfg := product.Config(t)
		cfg.Transform = compact.NewLocal(cfg.Model, compact.WithOnFold(rec.Fold)).Transform
		return cfg
	},
	Answer: engine.Answers,
}
```

A task file is JSON: an `instruction`, or `prompts` for several
turns, with `expect` for the judges and `setup` and `meta` for the
product. The suite's manifest, the hash of every file loaded, is
recorded in each run's `env` entry, so a result names the suite
version it came from.

## Replay

`replay.NewModel` serves a recorded session's model calls in path
order, folds included, and in strict mode refuses a request whose hash
differs from the recorded one: a hook, a transform or a front that
changes what the model would have been sent fails loudly against real
traffic. A record that carries no hash for a call is refused rather
than served unchecked — at `NewModel` when a response on the path is
unhashed, so a replay that could not have checked what it served says
so before it starts instead of passing quietly, and at the call for a
fold — unless `replay.AllowUnhashed()` says to serve it anyway. The
one call no mode checks is a fold through the compaction endpoint,
which sends no request the format hashes.
`replay.Tools` serves recorded tool outputs by call ID or by name and
canonical arguments, so a whole run replays without touching the
world.

A fold's own model call is told from a turn's by the mark agentturn's
`compact` package puts on its context (`compact.IsFold`, agentturn RFC
0001, The request), not by a request with no tools and no
instructions. A call a guard refused, recorded as
`agentturn:model_blocked`, is served again as the guard's refusal with
the usage the provider billed when the replayed run sends it, and
passed over when the replayed guard refuses it before the model is
called. The record says a refused call reached the model only by that
usage: agentturn writes no flag saying it was sent. So a refusal with
no usage is taken as a `BeforeModelCall` guard's, and a strict replay
that sends its request anyway diverges rather than serve a refusal the
model never made; a model guard's refusal the provider did not bill
reads the same way until agentturn records whether the call was sent. An output marked `failed` is served as the tool's failure, so
the replayed record keeps the mark, and a call carrying `copied_from`
is another session's: it is not indexed, and a call of this session
is never served its output. A sidecar session names its media by
`sidecar:` references, which no provider can fetch: `replay.WithBlobs`
gives the model and the tools the session's blob reader, through
which what they serve is resolved to data URLs and each request is
hashed in the form the session recorded it, and `FromSessionMedia`
resolves a task's prompts the same way; `FromSession` refuses such a
session with `session.ErrNoBlobReader`.

```go
s, _ := store.Open(ctx, id)
model, _ := replay.NewModel(s, replay.Strict())
cfg.Model = model
cfg.BeforeModelCall = model.BeforeModelCall
cfg.Tools = replay.Tools(s, cfg.Tools, replay.Strict())
```

`Model.BeforeModelCall` serves the instructions and the tool list
recorded for each call. A product whose layers rebuild those every
turn, from a memory store, a skill set or an AGENTS.md, otherwise
replays against what the layers say today and diverges at the first
call with an error naming two hashes and no layer. `Model.Settings`
is the rest of what each recorded call was made under, for a judge or
a check that wants to read it.

## Judges

`judge.Exact`, `judge.Contains` and `judge.ToolCalled` are
deterministic. `judge.Rubric` is an agent: the trajectory is its
input, the rubric its instructions, and its answer is constrained to a
score schema; with a store it records its own session, parented to the
one it judged, so a judgement is as replayable as the run.

## Samples and groups

`Runner.Samples` runs each task several times, each in its own
session, with `Result.Sample` and `Task.Meta["sample"]` saying which
run it was. `Runner.GroupJudges` score a task's samples together once
all of them are judged, as an RL producer computes advantages relative
to the group, and each score is an `outcome` on its sample's session.
A group with a sample that was not judged, because its run failed, is
not group judged. Each session's task record names the group judges it
expects, so a store tells a group that was never scored, because a
batch ended before its group step, from one that was.

## Comparison and cost

`Compare` runs a suite under two configurations, pairs the results by
task, and names how the two configurations differed, read back from
the sessions. The difference it names is the settings the record
describes; when those are the same and the two first requests are not,
it says so with `beyond_settings`, because a transform, a hook and an
injected item are not settings. A context strategy that folds only
later in the run shows as `folded`, whether each side's path held a
compaction entry, when one did and the other did not.

`CompareReports` compares two sets of stored results without running
anything: a candidate against a baseline a ref pins under
`eval/baseline/<task>` (RFC 0002, Refs), or two report files. A report
holds no fact its sessions lack, so each result is read again from its
store at the session and target it names, with `ResultAt`, which reads
any stored run back as a `Result`: its task, end, scores, usage and
cost. `Compare` is two runs followed by the same pairing.

`price` loads a table of rates and prices a run with ATIF's formula,
and the runner and the exporter take the same hook. The table is JSON,
the reader's own configuration: a model name, with or without its
provider prefix, to rates in US dollars per million tokens.

```json
{
  "gpt-5": {"input": 1.25, "cached": 0.125, "output": 10},
  "claude-sonnet-4-5": {"input": 3, "cached": 0.3, "output": 15,
    "cache_write": 3.75, "cache_write_by_ttl": {"5m": 3.75, "1h": 6}}
}
```

`cached` prices prompt tokens read from the provider's cache.
`cache_write` and `cache_write_by_ttl` price the tokens written to it,
which openresponses reports as the
`openresponses-experimental.cache_write_tokens` extension and its
split by TTL (docs/extensions.md section 5): each TTL's writes at that
TTL's rate when the usage splits them, else at `cache_write`, and the
rest of the uncached prompt at `input`, as the extension's Readers rule
asks. A table with no write rate prices writes as input. A result's `usage`,
`model_calls` and `cost_usd` sum every model call on the run's path as
`agentsession.ModelCalls` reads it, which is what the exported
document's final metrics sum, so the two agree on the total: every
response, the folds a compacting configuration made, and every
`custom` entry whose data carries a `usage`, as many calls as its
`attempts`, whatever its namespace (RFC 0001, `custom`): a fold that
failed, a call a model guard refused after the provider billed it
(`agentturn:model_blocked`), a product's own record. A run with a call
the hook cannot price has no `cost_usd`, where the document reports
the sum of the calls it could.

A run that delegated work to subagents costs that work too, as RFC
0001 (`link`) counts it and ATIF's `total_cost_usd` does: `cost_usd`
adds each session a `subsession` link on the path names, once, at the
leaf the call's output names or else the leaf a resume takes, each
call priced at the model in force in the child, the child's own
children included. `usage` and `model_calls` stay the path's own, as
the document's token totals and `total_steps` do, and `subagents`
holds the children's: how many sessions were counted, their usage,
calls and cost, which were counted at a resume leaf, and which could
not be read or priced and were left out rather than guessed at. The
children are read from the runner's store, so the document agrees with
the report when it is exported with `Subsessions` resolving there.

## Harbor

The `harbor` nested module reads a Harbor task directory as a `Task`
(the prompt only; the environment and the verifier stay with Harbor)
and a finished trial's `reward.json` or `reward.txt` as scores.
`Task.Meta` carries `[task]` and `[metadata]` under `task.` and
`metadata.`, because the second table is free-form and invites the
first table's words. Every other table goes into `Task.Setup` under
its own name, flattened with dots, and nothing here honours it. A
runner that builds the environment itself reads the keys `harbor.Load`
lists, as Harbor 0.25's `TaskConfig` defines them: the image or
Dockerfile, `cpus`, `memory_mb` and `storage_mb` (or the `memory` and
`storage` sizes Terminal-Bench 2.0 writes), `network_mode` (`no-network`,
`public` or `allowlist` with `allowed_hosts`), the MCP servers, and
the agent's and verifier's timeouts. The fixture under
`harbor/testdata/task` is a task `harbor lint` accepts.

`harbor.ImportJob` reads a finished Harbor job back. For each trial it
finds the agent's session in the store the agent recorded into under
the trial's `agent/` directory, the one session there that names no
parent, writes each of the trial's `verifier_result.rewards` onto it as
an `outcome`, and returns the job as a `Report`, which `CompareReports`
compares with another. Harbor leaves two choices open, and this module
makes them so:

- An imported reward is an outcome of kind `test`, the kind RFC 0001
  gives a verifier's score, not `eval`, which the runner's judges
  write: Harbor's reward is its verifier's verdict on the trial.
  Its `label` is `harbor/<reward key>`, so a reward is never taken
  for a judge of the same name, its `score` the reward's value, its
  `pass` a value of at least 1 unless `WithPass` says otherwise, and
  its `target` the run's last entry. `agenteval.ReadOutcome` reads
  both kinds.
- An errored trial with no reward, which Harbor counts as 0, is
  recorded as that 0, under `harbor/reward`, with no `pass`, which
  RFC 0001 reads as a verdict, and the exception in the details'
  `error`, so the session holds what Harbor counted and why.
  In the `Report` it is unjudged, with the exception on its error, and
  `ResultAt` reads it back the same way, so the 0 is not read as the
  verifier failing the agent; a reader that wants Harbor's mean counts
  it as 0 from the summary's `unjudged`. A trial that errored after
  its verifier wrote rewards keeps them, and carries the exception.

An import is idempotent: an outcome the session already holds is not
written again. Running a task, building its environment and running
its verifier, stays with Harbor, or with a product's Harbor adapter.

## Design

The plan is in `docs/plans/eval-layer.md` and the findings of the
design studies that shaped it in `docs/feedback.md`.

## License

MIT.
