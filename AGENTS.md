# agenteval

Evaluation for Go agents over Open Responses: a replay model and
recorded tools served from a session, tasks and suites loaded from an
`fs.FS`, a runner that records every run as a session, judges, and
scores written back as `outcome` entries. The design is in
`docs/plans/eval-layer.md`; read it before writing code. `docs/feedback.md` holds the design studies' findings against that plan; apply them to the plan before building the piece they touch.

## Module

- Module path: `github.com/ChristopherDavenport/agenteval`.
- Go 1.25 is the floor. The root package name is `agenteval`.
- The root module depends on
  `github.com/ChristopherDavenport/openresponses`,
  `github.com/ChristopherDavenport/agenttool`,
  `github.com/ChristopherDavenport/agentturn`, its `session` nested
  module, `github.com/ChristopherDavenport/agentsession` and the
  standard library. Nothing else. `make deps` and a test enforce it.
- `replay`, `judge` and `price` are packages of the root module;
  `harbor` is a nested module because it needs a TOML decoder.
- Pinned to the released siblings, not their working trees. Scores
  are written in the `outcome` shape RFC 0001 draft 0.2 defines,
  through `agentsession`'s own `Pass` field and `OutcomeEval` kind.

## Siblings

- `../open-responses`: the wire package. Copy its conventions.
- `../agenttool`: the tool contract, for `replay.Tools`.
- `../agentturn`: the loop, run by the runner and by the rubric judge;
  its `session` module records each run.
- `../agentsession`: the session format. Replay reads it, the runner
  writes it, scores are its `outcome` entries, and `export` renders
  the trajectories judges read.
- `../agentskill`, `../agentmemory`, `../agentpolicy`: configurations
  under test. Never imported here; a product's `Config` function
  wires them.

## Conventions

Mirror `../agenttool`: a `Makefile` with `build`, `deps`, `test`,
`vet`, `fmt`, `tidy`, `tidy-check`, `lint`, `vuln`, `check` and
`release` targets, the same CI shape, a `CHANGELOG.md` in Keep a
Changelog form, annotated `v*` tags. `make check` must pass before any
commit.

Tests are table-driven and offline. Replay fixtures are sessions
recorded from the `echo` adapter under `testdata/sessions/`; the
report has a golden under `testdata/report/`. Regenerate with `go test
. -update` and review the diff.
