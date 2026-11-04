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
  `harbor` is a nested module because it needs a TOML decoder, and
  `cmd/agenteval`, the command, is one because it needs `harbor`.
  In the tree the command replaces the root and `harbor` with
  `../../` and `../../harbor`, as `make replaces` requires; `go
  install pkg@version` refuses a module holding any replace, so the
  command is released in a second step with them dropped (see
  Releases).
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
`release` targets (here also `release-command` and `guard-test`), the
same CI shape, a `CHANGELOG.md` in Keep a
Changelog form, annotated `v*` tags. `make check` must pass before any
commit.

Tests are table-driven and offline. Replay fixtures are sessions
recorded from the `echo` adapter under `testdata/sessions/`; the
report has a golden under `testdata/report/`. Regenerate with `go test
. -update` and review the diff.

## Releases

In this workspace run make targets as
`GOWORK=off GOFLAGS=-mod=readonly make check`. A pushed tag is
permanent: `proxy.golang.org` and `sum.golang.org` keep the version
forever, so a bad version is superseded and `retract`ed, never deleted.
The user runs releases; prepare the changelog's *Unreleased* section
and hand over the commands.

A release is two steps at one version. The first:

```sh
make release VERSION=v0.1.0
```

points every nested module's first-party requires at the version
(`scripts/versions.sh`; the replaces make that resolve to the tree),
dates the changelog, tidies, runs `make check`, commits, guards and
tags the root and then each library (`harbor`) with
`scripts/release-guard.sh`, and pushes the branch and the tags with
`git push origin --atomic`. It does not tag the command. The second,
once the proxy serves the root and `harbor` at the version:

```sh
make release-command VERSION=v0.1.0
```

`scripts/release-command.sh` fetches origin and refuses, before changing
anything, unless it is on `main`, HEAD contains `origin/main`, no
`cmd/agenteval` tag of the version exists locally or on origin, and the
proxy serves the root and every library at the version; it asks
`https://proxy.golang.org` alone, with `GOPRIVATE`, `GONOPROXY` and
`GONOSUMDB` cleared and no `-mod` flag, as `go install` would.
`RELEASE_PROXY`, which its tests point at a file proxy, is honoured
only with `RELEASE_COMMAND_TEST=1` and a `file://` origin. It then drops
the command's replaces, points its first-party requires at the version,
tidies against the proxy, builds (to a temporary directory, so no binary
lands in the tree) and tests, commits the go.mod and go.sum alone,
guards and tags `cmd/agenteval/vX.Y.Z` from that commit, puts back the
go.mod and go.sum from before in a second commit, so the tree builds
the command against itself again, and pushes the branch and the tag
atomically. On a failure it prints the `git reset --hard` and `git tag
-d` that undo it. The guard refuses a command tag whose go.mod holds
any replace, one whose root version is not tagged before it, one not
requiring its first-party modules at its version, and one whose root or
`harbor` the proxy does not serve. On the tag, the release workflow runs
`go install github.com/ChristopherDavenport/agenteval/cmd/agenteval@vX.Y.Z`.
`make guard-test`, part of `check`, runs `scripts/release-command-test.sh`
against throwaway repositories and a file proxy.

`make extracted`, in CI, builds each nested module against the versions
it requires, the way a consumer gets it; the command is skipped while
the tree has moved on from the root and `harbor` releases it requires,
until `make release` publishes them. To check one tag alone,
`make release-guard TAG=v0.1.0` (or `harbor/v0.1.0`,
`cmd/agenteval/v0.1.0`).
