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
`vuln`, `test`, `tidy` and `published`.

The repository has two modules: the library at the root and `harbor`,
nested so its TOML decoder stays out of the library's dependency graph.
The Makefile targets cover both; a bare `go test ./...` at the root does
not. `deps` fails if the root module imports anything beyond
`openresponses`, `agenttool`, `agentturn`, `agentsession` and the
standard library; anything that needs another dependency is a nested
module listed in `SUBMODULES` in the Makefile. `lint` and `vuln` run
staticcheck and govulncheck through `go run`, which may download a
newer Go toolchain the first time.

`harbor`'s `go.mod` requires the released root next to a `replace` to
the tree, so a consumer fetches the version and the checkout builds
against the working copy. Go reads that `replace` when the module is
the main one, which is why the published module cannot be built from
its own zip; `make published` builds a copy of each nested module with
the `replace` dropped, against the released root, and CI runs it.

Tests are table-driven and offline. The replay fixtures under
`testdata/sessions/` are sessions recorded from the `echo` adapter and
the report golden under `testdata/report/` is written by the runner;
both are regenerated with `go test . -update`. Review the diff before
committing it.
