# Changelog

All user-visible changes to this library. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project
uses [Semantic Versioning](https://semver.org/); before v1.0.0 minor
versions may break the API.

## Unreleased

### Added

- `Runner.Build` hosts a configuration whose construction can fail or
  holds something to release, such as an agentkit kit: its error goes
  on `Result.Err` rather than surfacing as "config has no model", and
  the close it returns is called once the task is done. It wins over
  `ConfigWith` and `Config`. (#23)
- `Runner.SessionOptions` supplies the options each run's recorder is
  opened with, so a kit-built configuration records its instructions
  parts through `session.WithInstructionsParts`. (#23)
- `replay.ErrSubstituted` and `replay.AllowSubstitution()`. A strict
  replay refuses a path on which a later env entry names another
  workspace than the one in force before it, after a response, which
  RFC 0001 calls a substitution; the error wraps `ErrUnverifiable` and
  names the env entry. `replay.Unverifiable` reports it too, and takes
  `AllowUnhashed` and `AllowSubstitution` to ask about one rule alone.
  `ErrUnreplayable` no longer blames an unbound `WithOnFold` for a
  substitution. (#26)

### Changed

- Requires agentsession v0.0.11, agenttool v0.0.10 and agentturn and
  its `session` module v0.0.11. Runs are recorded as
  `agentsession/0.8`, and the callback given to
  `session.WithInstructionsParts` through `Runner.SessionOptions` now
  takes a `context.Context` first.
- A strict replay's substitution check is `agentsession.SameWorkspace`,
  so members of a workspace the format does not define, such as its
  host, count: the same image resumed on another host is a
  substitution.

### Fixed

- A replayed tool reports every property the original declares:
  `replay.Tools` wraps each tool with `agenttool.Wrap`, so confinement,
  resource, annotations, replay safety and `io.Closer` reach a policy
  and the executor, and a strict replay under the live policy no
  longer asks where the live run did not. (#22)
- `replay.WithLeaf`, `NewModel`, `Unverifiable` and `Tools` resolve an
  entry ID from before the 0.5 migration through its `legacy_id`, as
  `export.At` does. (#24)
- A task whose `Header` names a `Base` starts its agent from the
  context at the base, which `session.Start` seeds the recorder with,
  so the fork's requests are hashed and it replays strictly; it was
  run unseeded and reported `ErrUnreplayable`. (#25)

## v0.0.5 - 2026-09-28

### Added

- A result `replay.Tools` serves carries the record its call's result
  carried as its `Details`, so a wrapper that acts on a result's
  details acts on a replayed call as it did on the live one: a grant
  made on a skill read is made again, and a replay no longer asks where
  the record did not. The record is the tool's custom entry beside the
  call, found by its `call_id` and, in a session from before
  `agentsession/0.6`, by position when one call alone was waiting for
  its output. It is served as a `replay.Record`, which is itself
  `Recordable`, so the replayed session holds the record again beside
  the served call; `replay.DetailsAs[T]()` serves the records of T's
  namespace decoded as a T, for a wrapper that asserts its type. A
  nested call's record is not its parent's; a record the tool wrote
  through `agenttool.WriteRecord` names the call as its result's does
  and cannot be told from it, so a tool whose details are not
  recordable and that wrote one is served that one. A
  wrapper the product builds inside the tools it hands over, as
  agentkit's granting wrapper is, still sits under the replay and sees
  nothing; the seam to wrap each tool where it is made is agentkit's,
  filed there. (#17)
- `Result.ResumeBound` says a prompt was still waiting on input when
  `MaxResumes` stopped resuming it, so a task the bound ended no longer
  reads like one whose answer source declined. (#20)
- `ConfigDiff.Folded` says one run's path held a compaction entry and
  the other's none, so `Compare` of a plain configuration against a
  compacting one no longer reports that nothing differed when the
  first calls were the same. `Empty` counts it. Whether a run folds
  depends on the task, so it does not make a suite's pairs
  non-uniform; the comparison's own `Folded` is over every run of
  each side. (#20)

### Fixed

- `Result.Usage` and `Result.CostUSD` count the folds a compacting
  configuration made and any branch summary on the path, pricing each
  under the model the exporter prices it under, so the report's cost
  column and the exported document's `final_metrics` agree on the
  total whenever every call was priced. They summed the responses alone, while the doc comment said
  the folds were included: a compacting run reported 38 per cent of
  what it spent in the harbor-eval study, and `Compare` could call the
  costlier configuration the cheaper. (#18)

### Notes

- The `replace => ../` in `harbor`'s published `go.mod` stays, since the
  release builds the pair from one commit through it, and the pair is
  tested: `make extracted` copies `harbor` out of the tree, drops the
  replace and builds, vets and tests it against the root its require
  names, and CI runs it on every pull request and on main. It ran
  against the published pair on the v0.0.4 release commit and passed.
  What cannot build is the module cache's copy in place, where the
  relative replace names a directory that is not there; a consumer
  never builds it that way, since Go ignores a dependency's replace.
  `harbor/go.mod` now says this beside the replace. (#19)

### Dependencies

- agentturn and its `session` module v0.0.9 to v0.0.10, agenttool
  v0.0.8 to v0.0.9, and agentsession v0.0.8 to v0.0.9, in the root
  module and in `harbor`. No API of this module changes with them. The
  runner records `agentsession/0.6`, which adds optional members only:
  a run start carries its trigger, a custom record names its call, and
  the header promises `queued`. The fixtures under `testdata/sessions`
  are rewritten in that format.

## v0.0.4 - 2026-09-28

### Dependencies

- agentturn and its `session` module v0.0.8 to v0.0.9, agenttool
  v0.0.7 to v0.0.8, and agentsession v0.0.7 to v0.0.8, in the root
  module and in `harbor`. No API of this module changes with them. The
  sessions the runner records are `agentsession/0.5`, whose entry IDs
  are the hashes of their envelopes, so a `Result.Target` and the
  outcome entries it names carry a `sha256:` hash where they carried
  the ID the store chose, and a `dispatch` entry is written per call as
  it reaches its tool rather than per batch. The fixtures under
  `testdata/sessions` are rewritten in that format.

## v0.0.3 - 2026-09-24

### Dependencies

- openresponses v0.0.9 to v0.0.12, agenttool v0.0.5 to v0.0.7,
  agentturn and its `session` module v0.0.6 to v0.0.8, and agentsession
  v0.0.5 to v0.0.7, in the root module and in `harbor`. No API of this
  module changes with them.

## v0.0.2 - 2026-09-23

### Added

- `Runner.ConfigWith`, the configuration function with the recorder
  that writes the run's session, preferred over `Config` when set. A
  compacting configuration binds `compact.WithOnFold(rec.Fold)` here,
  without which its folds reach no session and the run cannot be
  replayed strictly; a layer that wants to annotate the run it is in
  keeps the same recorder for `Recorder.Annotate` (#1, #7).
- `Runner.Answer` and `Runner.MaxResumes`: a run that ends
  `input_required` is resumed with the answers `Answer` returns, up to
  `DefaultMaxResumes` times, and every resume is recorded, so an
  evaluation of a product whose policy asks measures the whole run and
  not the part before its first ask. `agentpolicy.Engine.Answers`
  satisfies the signature as written, and `Result.Resumes` counts the
  resumes (#7).
- `ErrUnreplayable`, joined onto the result of a run whose session
  cannot be replayed strictly because a response on its path carries
  no request hash. The runner reports it when the run is written
  rather than leaving it to a replay weeks later (#1).
- `replay.ErrUnverifiable`, `replay.AllowUnhashed()` and
  `replay.Unverifiable`. A strict replay refuses a call the record
  carries no hash for rather than serve it unchecked: `NewModel`
  refuses the whole session when a response on the path is unhashed,
  before a call is served, and a fold whose compaction entry recorded
  no fold hash is refused at the call, since whether that matters
  depends on the compactor the replay runs with. `AllowUnhashed()`
  serves them by position instead, which is how a recording made
  before agentturn v0.0.6 replays, and its doc says what it turns off.
  `Unverifiable` is the rule `NewModel` applies, exported so a caller
  can ask before building a model; `ErrUnreplayable` is now that rule
  plus the runner's diagnosis rather than a second copy of it. A fold
  through the compaction endpoint is the one call none of this governs
  — it sends no request the format hashes, so `Model.Compact` serves it
  unchecked in every mode, which the package doc, `Strict`, `Served`
  and the README now say (#2).
- `Served.Recorded` is set for a fold served through `Model.Compact`
  when the compaction entry records a fold hash. The endpoint cannot
  check it, so it is reported rather than compared, and `Got` stays
  empty: a model call was checked exactly when both are set. A tool
  call is reported only when a recorded output was found, so its
  `Match` is always true and its hashes are always empty.
- `replay.Model.BeforeModelCall`, an `agentturn` `BeforeModelCall`
  hook that serves the instructions and the tool list in force at each
  recorded call, and `Model.Settings` and `Model.SettingsAt`, the
  settings every recorded step was made under. A product whose layers
  rebuild the instructions each turn from a store, a skill set or a
  memory block replays strictly with the hook chained and diverges at
  its first call without it (#5).

### Changed

- **Breaking.** `harbor.Load` prefixes `[task]` and `[metadata]` with
  their own table's name in `Task.Meta`, as `Setup` already prefixes
  every other table: the keys are now `task.name` and
  `metadata.name`. Flattened unprefixed, a task carrying the same key
  in both tables lost one of the two values, and which one it lost
  depended on Go's map iteration order. A consumer reading `Meta`
  updates its keys (#3).
- **Breaking.** `replay.Strict()` checks a local fold's own request
  against the hash its compaction entry records, and refuses a
  mismatch with
  `ErrDiverged` as it does for a response, so a summary prompt, a
  budget or a filter that changed is no longer answered with the
  recorded summary and reported as neutral. `Served` carries both
  hashes for a fold. A compaction entry with no recorded fold hash, a
  recording made before agentturn v0.0.6, is refused as
  `ErrUnverifiable` unless `AllowUnhashed()` is set; a fold through the
  compaction endpoint sends no request to check and is served through
  `Compact` as before. A replay that passed while folding differently
  from the recording now fails, which is the point (#2).
- **Breaking.** `judge.Render` strips the raw item passthrough through
  `export.NoPassthrough`, so a judged document keeps the root's
  payload profile name and says which wire profile produced it.
  `judge.StripRaw` is that function under this package's name and is
  deprecated; the two did one job twice and disagreed about that one
  member (#4).
- `Compare` reports `ConfigDiff.BeyondSettings` when two runs' first
  calls sent different requests although their settings were the same,
  which is a transform, a hook or an injected item. A comparison of
  two context strategies reported that nothing differed (#4).
- `harbor`'s package comment holds the Harbor verifier judge, twelve
  lines wrapping `Reward`, and `Load` says that every real Harbor task
  ID holds a slash, since Harbor validates a name as `org/name` (#4).
- The README and `Result.Usage` say that the runner and the exported
  document price the same calls over different scopes, the whole path
  against the context after the last compaction, so the two agree on
  the rate and not on the total (#4).

### Fixed

- A task result no longer names this package twice in front of one
  message. `Run` wraps each failure with `agenteval: task <id>: `, and
  the two errors on that path that come from this package —
  `ErrUnreplayable` and the one `trajectoryAt` returns — already began
  with `agenteval: `. Those two sites now wrap with `task <id>: ` and
  leave the naming to the error, and the sites that wrap another
  package's error, or one with no name of its own, are unchanged.
- `replay` serves a recorded item's own bytes. It streamed each item
  through `openresponses.Emitter`, which promotes an unset status to
  `completed` as it closes an item, so a recording from a server that
  leaves an optional field unset, as Ollama does the status of a
  reasoning item, could never replay: the added field changed every
  later request and the divergence named two hashes and no field. The
  two item events are sent directly and the emitter keeps the output
  indices, the response snapshot and the terminal event (#6).
- `replay` reads a response's output items by the contiguity rule
  `agentsession` is adopting: entries that are not item entries are
  skipped, and the walk stops at the first item entry whose response
  ID differs. A custom entry written between two output items of one
  response, which is where a guard or a policy layer writes its
  verdict, no longer costs the response the items before it.
- `replay.Strict()` no longer reports a call the record carries no hash
  for as a divergence. It compared the received hash against the empty
  string, so a replay under the recorded configuration failed with a
  message describing a mismatch that never happened: `recorded ,
  received sha256:…`. Such a call is refused as `ErrUnverifiable`
  instead, which says what is actually wrong — the record cannot
  describe the request — and `NewModel` refuses it at construction
  rather than at the call.

## v0.0.1 - 2026-09-20

- Initial release: `replay.Model` and `replay.Tools` serve a recorded
  session's model calls, folds and tool outputs; `Task`, `Suite`,
  `LoadSuite`, `FromSession` and the manifest; the judge contract with
  `judge.Exact`, `judge.Contains`, `judge.ToolCalled` and
  `judge.Rubric`; `Runner`, scores written as `outcome` entries,
  `Report` and `Compare`; `price` for costing a run; and the `harbor`
  nested module reading a Harbor task and a verifier reward.
- Depends on `openresponses` v0.0.9, `agenttool` v0.0.5, `agentturn`
  v0.0.6 with its `session` module and `agentsession` v0.0.5, the
  release that implements RFC 0001 draft 0.2: a score is written with
  the format's own `pass` member and `eval` kind, and targets the
  `run` end record the recorder writes, the last entry of the run
  judged.
