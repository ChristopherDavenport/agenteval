# Changelog

All user-visible changes to this library. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project
uses [Semantic Versioning](https://semver.org/); before v1.0.0 minor
versions may break the API.

## Unreleased

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
