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

The individual targets are `fmt`, `tidy-check`, `vet`, `deps`,
`no-replace`, `lint`, `vuln`, `test` and `tidy`.

The repository has two modules, joined by `go.work`: the library at the
root and `harbor`, nested so its TOML decoder stays out of the library's
dependency graph. The Makefile targets cover both; a bare `go test ./...`
at the root does not, even in workspace mode. `deps` fails if the root
module imports anything beyond `openresponses`, `agenttool`, `agentturn`,
`agentsession` and the standard library; anything that needs another
dependency is a nested module listed in `SUBMODULES` in the Makefile.
`lint` and `vuln` run staticcheck and govulncheck through `go run`, which
may download a newer Go toolchain the first time. Workspace mode rejects
`-mod=mod`, so a `GOFLAGS=-mod=mod` in your environment has to go.

A nested module's `go.mod` requires a released root version and carries
no `replace`: the workspace is what builds it against the tree. That is
deliberate. A `replace` is a property of the main module and consumers
ignore it, so a nested module that carried one would build green here
while shipping a `go.mod` that names a root version without the API it
uses. `make release-check` builds each nested module with `GOWORK=off`,
against the versions its own `go.mod` requires, which is what a consumer
gets.

`release-check` is not part of `check`, and it fails by design between a
root API addition and the next root tag — a nested module that uses the
new API cannot name a version that carries it until that version exists.
That failure is the release ordering, not a bug; see below.

`make no-replace` is what keeps `release-check` honest, and it *is* part
of `check` and of CI. Re-adding a `replace` makes every other gate green
again after one `make tidy` — including `release-check`, on a module no
consumer can build — so the absence of one is asserted on every run
rather than only at release.

Tests are table-driven and offline. The replay fixtures under
`testdata/sessions/` are sessions recorded from the `echo` adapter and
the report golden under `testdata/report/` is written by the runner;
both are regenerated with `go test . -update`. Review the diff before
committing it.

## Releases

Every module in the repository shares one version, but not one commit.
A nested module's requirement cannot name a tag that does not exist
yet, so the root is released first and the nested modules follow. With
the changelog's *Unreleased* section written:

```sh
make release-root VERSION=v0.1.0
```

dates the changelog, runs `make check`, commits, tags `v0.1.0` with the
changelog section as the message, and pushes the branch and the tag.
The nested modules still require the previous root release across this
commit, which is correct: `v0.1.0` did not exist when it was written.

Once that tag is on the module proxy:

```sh
make release-submodules VERSION=v0.1.0
```

walks `SUBMODULES` in order and, for each, sets its requirement on the
root and on any already-released sibling to the version, tidies, builds
and tests it with `GOWORK=off` against exactly those versions, commits,
tags `<dir>/v0.1.0` and pushes. One commit and one tag per module,
because `go mod tidy` and the `GOWORK=off` build both resolve a sibling
requirement from the proxy: a module has to be published before the
module that requires it is bumped. A module is built the way a consumer
builds it before its tag is written, and the release workflow runs
`make release-check` again on the tag — scoped to that tag's module,
since the others are still on the previous root at that commit.

If a module fails partway, the tags already pushed stay valid and
self-consistent; nothing has to be deleted. The failure will usually
have left that module's `go.mod` and `go.sum` rewritten, so:

```sh
git checkout -- harbor                       # the module that failed
make release-submodules VERSION=v0.1.0 RELEASE_SUBMODULES=harbor
```

Narrow `RELEASE_SUBMODULES`, never `SUBMODULES`: the first is the list to
release, the second is the list of siblings to bump, and narrowing the
second would tag a module still requiring an old sibling — which
`release-check` cannot catch, because the old sibling satisfies it.
`release-submodules` refuses a `SUBMODULES` override for that reason.
`harbor` is the only nested module today, so there is no sibling to
bump; the target is the same one `agenttool` uses, where there is.

The release workflow publishes a GitHub release per tag, and the Go
module proxy picks the versions up. Before v1.0.0 the API may change
between minor versions; the changelog records every break.
