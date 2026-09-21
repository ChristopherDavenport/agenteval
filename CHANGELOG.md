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
