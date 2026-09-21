# Changelog

All user-visible changes to this library. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/) and the project
uses [Semantic Versioning](https://semver.org/); before v1.0.0 minor
versions may break the API.

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
