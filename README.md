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
recorder. `Answer` answers the calls a run left pending when it ended
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
traffic. A record that carries no hash for a call is refused at
`NewModel`, so a replay that could not have checked what it served
says so before it starts instead of passing quietly.
`replay.Tools` serves recorded tool outputs by call ID or by name and
canonical arguments, so a whole run replays without touching the
world.

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

## Comparison and cost

`Compare` runs a suite under two configurations, pairs the results by
task, and names how the two configurations differed, read back from
the sessions. The difference it names is the settings the record
describes; when those are the same and the two first requests are not,
it says so with `beyond_settings`, because a transform, a hook and an
injected item are not settings.

`price` loads a table of rates and prices a run with ATIF's formula,
and the runner and the exporter take the same hook, so the two agree
on the rate. They do not agree on the total: a result's `usage` is
every response on the run's path, while the exported document's final
metrics cover the context after the last compaction, so a run that
folded is counted twice over on two different scopes.

## Harbor

The `harbor` nested module reads a Harbor task directory as a `Task`
(the prompt only; the environment and the verifier stay with Harbor)
and a finished trial's `reward.json` or `reward.txt` as scores.
`Task.Meta` carries `[task]` and `[metadata]` under `task.` and
`metadata.`, because the second table is free-form and invites the
first table's words.

## Design

The plan is in `docs/plans/eval-layer.md` and the findings of the
design studies that shaped it in `docs/feedback.md`.

## License

MIT.
