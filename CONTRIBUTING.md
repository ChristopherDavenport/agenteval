# Contributing

Issues and pull requests are welcome.

## Before you start

The module is the evaluation layer over the Agent Session Format: it
runs `agentturn` to produce `agentsession` records, replays them, judges
them and writes scores back as `outcome` entries. The design is in
`docs/plans/eval-layer.md` and the findings of the design studies that
shaped it are in `docs/feedback.md`. Read both before writing code.

Scores are written in the shape RFC 0001 of `agentsession` defines for
an `outcome` entry; a change to what a score looks like on disk starts
as a change to that RFC.

For anything larger than a bug fix, open an issue first so the shape of
the change can be discussed before you spend time on it.

## Development

Go 1.25 or later is required. The full local check is:

```sh
make check        # gofmt, tidy, vet, deps, staticcheck, govulncheck, race tests
```

The individual targets are `fmt`, `tidy-check`, `vet`, `deps`, `lint`,
`vuln`, `test` and `tidy`.

The repository has two modules: the library at the root and `harbor`,
nested so its TOML decoder stays out of the library's dependency graph.
The Makefile targets cover both; a bare `go test ./...` at the root does
not. `deps` fails if the root module imports anything beyond
`openresponses`, `agenttool`, `agentturn`, `agentsession` and the
standard library; anything that needs another dependency is a nested
module listed in `SUBMODULES` in the Makefile. `lint` and `vuln` run
staticcheck and govulncheck through `go run`, which may download a
newer Go toolchain the first time.

Tests are table-driven and offline. The replay fixtures under
`testdata/sessions/` are sessions recorded from the `echo` adapter and
the report golden under `testdata/report/` is written by the runner;
both are regenerated with `go test . -update`. Review the diff before
committing it.

## Releases

Every module in the repository is released at one version, from one
commit, and requires its first-party siblings at exactly that version.
With the changelog's *Unreleased* section written:

```sh
make release VERSION=v0.1.0
```

points `harbor` at the version, dates the changelog, runs `make tidy`
and `make check`, reads the requires back to confirm tidy did not move
them, commits, then guards and tags `v0.1.0` and `harbor/v0.1.0`, and
pushes the branch and both tags with `git push origin --atomic`.

`make release-guard TAG=<tag>` is what stands between a mistake and a
permanent one, and `make release` runs it for every tag it writes. It
refuses a dirty tree, a tag that already exists locally or on origin, a
version that sorts below the current root release or does not move its
module forward, a first-party require that does not name that version, a
root tag that is not this commit, and a module that will not build with
`GOWORK=off`. The root is guarded and tagged first, because a nested
module's guard needs the root tag to exist. Nothing is public until the
push, so a refusal costs a `git reset --hard HEAD~1` and a `git tag -d`.

`make replaces`, part of `check`, refuses a first-party require that
lacks a matching `replace`. There is no `go.work` here, so the replaces
are the only thing building the tree against itself — and losing one
would make the next release resolve that module from the proxy, where
the version being released does not exist yet.

`go mod tidy` can move a requirement that the release just set, so the
requires are read back and asserted before anything is tagged.
