# Feedback from the design studies

Findings the design studies under `../examples` raised against this
module, held here because it has no issue tracker yet. Each section is
one finding in the shape of an issue: why it matters, the evidence, a
failure scenario and the smallest fix. The plan in `plans/` is the
document these correct; apply them there before the code is written,
and move each section to an issue when a repository exists.

Generated 2026-09-20 from the studies' issue drafts.

## 1. plan: the eval-layer's outcome mapping does not work as written


### Why this is necessary

`docs/plans/eval-layer.md` says each judge's score is appended as an `outcome`
entry with `kind: "eval"`, the judge's name as `label`, the task ID as `target` and
the judge's details, "so `export.PreferScore` chooses branches by it with no new
code". Built that way, none of the branch selection works and a session's scores
cannot be read back without knowing each judge's private JSON. Because the plan
names this as the mechanism that needs no new code, the mistake would be
discovered only after the runner, the judges and the report were all written
against it.

### Evidence

At agentsession `65a5714`:

- `export/trajectory.go:108-142` `PreferScore` resolves an outcome's `Target`
  through `childHolding`, which at `export/trajectory.go:145-159` walks
  `s.Path(id)` for one of the fork's children. A task ID is on no path, so it
  counts for no child.
- `export/atif.go:532` has the same dependency, so an outcome reaches the step it
  judges only when `stepByEntry[o.Target]` hits.
- `entry.go:38-44` the kinds are `feedback`, `test`, `task`, `tool_error` and
  `custom`. `eval` is not among them, and nothing validates `Kind`.
- `entry.go:257-264` `OutcomeEntry` has no `Pass` and no `Reason`, while the plan's
  `judge.Score` has both.
- `export/trajectory.go:104-142` takes each branch's maximum score rather than its
  mean, so a report that averages judges and `PreferScore` will name different
  winners.

A probe over a forked session shows `PreferScore` returning "" at every fork with
task IDs as targets and picking the higher-scoring branch with entry IDs, and shows
a second judge scoring 1.0 on the lower branch flipping the preference.

Separately, Harbor's verifier reward is `dict[str, float | int]` with only a
finiteness check, so a Harbor score of 2.5 or -1 is legal there while
`judge.Score.Value` is specified "0 to 1".

### Failure scenario

The runner writes one outcome per judge with the task ID as `target`. The report
computed in memory looks right, and every scored session offers no branch
preference to any tool that reads it back, with nothing having errored. A second
tool asked for the suite's pass rate from the session files alone cannot produce
one, because pass lives inside `details` under a per-judge schema.

### Suggested fix

Correct the plan before the code: `Target` names the last entry of the run being
judged and the task ID moves into `Details`; the kind is `test` for a deterministic
judge and `feedback` for a rubric judge until an `eval` kind exists; `Score.Value`
is documented as comparable rather than normalised, with the raw verifier number
kept beside it. The plan must **not** keep claiming branch preference works with no
new code. `Pass` and `Reason` need either fields on the entry or a `details` schema
fixed in the RFC, and the rubric judge's own session has no link relation meaning
"judged by".

Found by the harbor-eval design study against Harbor and mini-SWE-agent.

## 2. plan: a Harbor task as a Task is nearly empty; the reward is the direction that pays


### Why this is necessary

`docs/plans/eval-layer.md` leaves open "whether a Harbor adapter, the shape that
turns a Harbor task into a `Task` and a run into its expected output, belongs here".
Read against Harbor as it is, the first half of that shape carries almost nothing
and the second half carries the score. Building it in the order the question implies
produces an adapter that looks like it works, loads a real benchmark, and scores
runs that have nothing to do with what Harbor would have reported.

### Evidence

A Harbor task is `instruction.md`, `task.toml`, `environment/Dockerfile`,
`solution/solve.sh` and `tests/test.sh`. Of that, only the instruction and a few
`task.toml` keys survive into a `Task`, because the plan's own non-goals put
sandboxes and environments on Harbor's side. What is lost is the Dockerfile, the
network policy, the CPU, memory, storage and GPU requests, the MCP server
declarations, the skills directory, the healthcheck, the oracle solution, the
multi-step `[[steps]]` structure with its `multi_step_reward_strategy` and per-step
`min_reward`, and the verifier. The verifier is the largest loss. `Task.Expect` is
judge-defined JSON, while a Harbor task's notion of success is a shell script that
runs in a container after the agent stops.

Two details make the mapping thinner than it looks. `[metadata]` is an untyped
`dict[str, Any]`, so difficulty, tags and category are conventions Harbor's
scaffolder writes rather than schema, and `[task].name` is the only required field
in the whole file.

The reward direction is a clean fit. A Harbor verifier writes one number to
`/logs/verifier/reward.txt` or an object of labelled numeric metrics to
`/logs/verifier/reward.json`, with the JSON preferred when both exist, and the
score lands in the trial's `result.json` at `verifier_result.rewards.reward`. A
labelled numeric metric is `judge.Score` with the key as `Judge` and the number as
`Value`, and it is exactly what an `outcome` entry holds. Those values are unbounded
and only checked for finiteness.

### Failure scenario

Someone writes the task loader first, points it at a real Harbor dataset, and runs
the suite through the runner outside a container. Every task loads, every run
produces a trajectory, and the deterministic judges score them against `Expect`
fields nobody filled in. The numbers look like benchmark results. The verifier never
ran and the agent had no repository to work in.

### Suggested fix

Name the two directions separately and build the reward one first. A
`Reward(fsys, dir) ([]judge.Score, error)` over a finished trial directory turns
Harbor's verifier into a judge, and a `Load(fsys, dir) (agenteval.Task, error)`
reads the instruction and copies `task.toml` into `Setup` and `Meta` verbatim. The
fix must **not** pretend to honour what it copies. Document that a task loaded this
way is the prompt only, and that running it faithfully means running it under
Harbor. It belongs in a nested module, because `task.toml` needs a TOML decoder and
the root module's dependency rule forbids one.

Found by the harbor-eval design study against Harbor and mini-SWE-agent.

## 3. plan: nothing in the stack can price a run, and three consumers need the number


### Why this is necessary

Harbor reports a cost column per run, and mini-SWE-agent caps a run at three
dollars by default. No module in the stack can produce the number either one needs.
The hook exists and nothing fills it, so a product that runs an agent under Harbor
cannot report what a run cost, cannot cap what a run may spend, and cannot match the
budget the agent it is compared against runs under. The plan should say where the
price table lives before the runner and the report are built around its absence.

### Evidence

`export.Options.Cost` at agentsession `65a5714`, `export/atif.go:23`, is the only
cost-shaped declaration in any sibling. A grep across openresponses, agenttool,
agentturn and agentsession for cost, price or usd finds that hook, the two ATIF
fields it fills, and one unrelated comment. `openresponses.Usage` at
open-responses `657359f`, `response.go:306`, carries token counts and no price. A
probe that exports a recorded session with no `Options.Cost` shows
`final_metrics.total_cost_usd` absent.

Three consumers need it:

- ATIF defines `metrics.cost_usd` with the formula
  `(prompt - cached) x in + cached x cached + completion x out`. The hook receives
  enough to apply it, since `Usage` carries `InputTokensDetails.CachedTokens`.
- Harbor derives its own cost column from the exported document, and its
  `compute_model_usage` reports a model's cost as null when any token-bearing step
  of that model lacks `cost_usd`, so an unset hook leaves the cost column null
  rather than low.
- mini-SWE-agent's default budget is a `cost_limit` of three dollars, checked before
  every model call. `agentturn` at `2fd849e` has `MaxTurns` (`config.go:115`) and no
  cost limit, and `ShouldStopAfterTurn` (`config.go:135`) runs after a turn
  (`run.go:381`), so a limit built on it overshoots by one call. mini-SWE-agent
  treats an unpriced model as fatal unless cost tracking is explicitly set to
  ignore errors; a Go agent with no price table has no such switch to throw.

### Failure scenario

A product is asked to run its agent against a Harbor dataset alongside
mini-SWE-agent on the same three-dollar budget. It has token counts and no prices,
so either the run is unbudgeted, or a price table is hard-coded inside the agent and
the exported document's cost disagrees with whatever the harness reports. Harbor's
own cost column is null either way.

### Suggested fix

A nested module, `agenteval/price`, with a table loaded from an `fs.FS`, a
`For(model string) (Rates, bool)` lookup and a `Hook(t Table)` shaped for
`export.Options.Cost`, so the report and the exported document agree on one number.
It must **not** go in openresponses, which is the wire, or in agentsession, which is
the record. Prices change, and they belong with the thing that compares runs. Capping
spend needs a small agentturn change as well, a `CostLimit` checked before the model
call the way `MaxTurns` is checked before the turn, and that may be worth its own
issue.

Found by the harbor-eval design study against Harbor and mini-SWE-agent.

