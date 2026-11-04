#!/usr/bin/env bash
# The second step of a release (AGENTS.md, Releases): once make release
# has tagged the root and its libraries at VERSION and the proxy serves
# them, release each command module at the same version, with no
# replace, so go install takes it.
#
#   scripts/release-command.sh v0.1.0 cmd/agenteval
#
# For each command it drops the replace directives, points every
# first-party require at VERSION, tidies against the proxy, builds and
# tests, commits, guards and tags <dir>/VERSION from that commit; then
# it restores the command's go.mod and go.sum as they were before, in a
# second commit, so the tree builds the command against itself again, and pushes the branch and the tags in
# one atomic push. Nothing is pushed if any step fails: git reset --hard
# to the commit before, and git tag -d the tags written.
set -Eeuo pipefail

die() { echo "release-command: $*" >&2; exit 1; }

# The public proxy alone answers, as it answers go install: a GOPRIVATE
# or GONOPROXY match would otherwise read origin directly and pass a
# version the proxy does not serve. RELEASE_PROXY is for the tests
# alone: it is honoured only with RELEASE_COMMAND_TEST=1 and an origin
# that is a file:// remote, a throwaway repository, and refused
# otherwise, so one leaking in from the environment cannot have go.sum
# written from an unverified source.
if [ -n "${RELEASE_PROXY:-}" ]; then
  [ "${RELEASE_COMMAND_TEST:-}" = 1 ] || die "RELEASE_PROXY is set outside a test (RELEASE_COMMAND_TEST=1); unset it"
  case "$(git remote get-url origin 2>/dev/null)" in
    file://*) ;;
    *) die "RELEASE_PROXY is honoured only for a file:// origin, a test's" ;;
  esac
fi
export GOPROXY="${RELEASE_PROXY:-https://proxy.golang.org}" GOPRIVATE= GONOPROXY= GOFLAGS="${GOFLAGS:+$GOFLAGS }"
[ -n "${RELEASE_PROXY:-}" ] || export GONOSUMDB= GOSUMDB=sum.golang.org
GOFLAGS="$(printf '%s' "$GOFLAGS" | sed -E 's/-mod=[a-z]+//g')"
export GOFLAGS

VERSION="${1:-}"; shift || true
[ -n "$VERSION" ] && [ $# -gt 0 ] || die "usage: $0 <version> <command-dir>..."
HERE="$(cd "$(dirname "$0")" && pwd)"
[ -z "$(git status --porcelain)" ] || die "working tree is not clean"

# Everything below is checked before anything is changed.
[ "$(git symbolic-ref -q --short HEAD)" = main ] || die "not on main: a release is cut from main alone"
git fetch -q origin main --tags || die "cannot fetch origin"
git merge-base --is-ancestor origin/main HEAD \
  || die "HEAD does not contain origin/main: pull first"
git rev-parse -q --verify "refs/tags/$VERSION" >/dev/null || die "the root is not tagged $VERSION: run make release first"
remote_tags="$(git ls-remote --tags origin 2>/dev/null | sed -e 's|.*refs/tags/||' -e '/\^{}$/d')"
for c in "$@"; do
  [ -f "$c/go.mod" ] || die "no module at $c"
  ! git rev-parse -q --verify "refs/tags/$c/$VERSION" >/dev/null || die "$c/$VERSION is tagged already"
  ! printf '%s\n' "$remote_tags" | grep -qxF "$c/$VERSION" || die "$c/$VERSION is on origin already"
done

# The root and every library at VERSION must be on the proxy, or the
# tidy below fails half way.
on_proxy() {
  local err
  err="$(cd "$(mktemp -d)" && GOWORK=off go list -m -e -f '{{with .Error}}{{.Err}}{{end}}' "$1@$VERSION" 2>&1)"
  [ -z "$err" ] || die "$1@$VERSION is not on the proxy yet: $err"
}
on_proxy "$(sed -n 's/^module[[:space:]]*//p' go.mod | head -1)"
for lib in $(git ls-files '*/go.mod'); do
  dir="${lib%/go.mod}"
  skip=""
  for c in "$@"; do [ "$dir" = "$c" ] && skip=1; done
  [ -n "$skip" ] || on_proxy "$(sed -n 's/^module[[:space:]]*//p' "$lib" | head -1)"
done

START="$(git rev-parse HEAD)"
written=""
# Printed once, by the script's own shell, not again by each subshell
# that failed inside it.
trap '[ "$BASH_SUBSHELL" = 0 ] && echo "release-command: failed; undo with: git reset --hard $START${written:+; git tag -d$written}" >&2' ERR

for c in "$@"; do
  # The module path of each replace, old=new, from the JSON go mod edit
  # writes.
  pairs="$(cd "$c" && GOWORK=off go mod edit -json | awk '
    /"Replace"/ { in_r = 1 }
    in_r && /"Old"/ { want = "old" }
    in_r && /"New"/ { want = "new" }
    in_r && /"Path"/ && want != "" {
      gsub(/[",]/, "", $2); if (want == "old") o = $2; else { print o "=" $2 }; want = ""
    }')"
  for p in $pairs; do
    (cd "$c" && GOWORK=off go mod edit -dropreplace="${p%%=*}")
  done
  "$HERE/versions.sh" set "$VERSION" "$c"
  # go build ./... matching a single main package writes the binary
  # into the module's directory, where the add below would commit it
  # into the release; -o to a directory of its own keeps it out.
  bin="$(mktemp -d)"
  (cd "$c" && GOWORK=off GOFLAGS=-mod=mod go mod tidy && GOWORK=off GOFLAGS=-mod=readonly go build -o "$bin" ./... && GOWORK=off GOFLAGS=-mod=readonly go test ./...)
  rm -rf "$bin"
done
# The go.mod and go.sum alone: anything else the steps above left would
# otherwise go into the tag, and the guard's clean-tree check refuses it.
for c in "$@"; do git add -- "$c/go.mod" "$c/go.sum"; done
git commit -q -m "Release $* $VERSION"

tags=""
for c in "$@"; do
  "$HERE/release-guard.sh" "$c/$VERSION"
  notes="$("$HERE/release-notes.sh" "$VERSION")"
  git tag -a "$c/$VERSION" -m "$notes"
  tags="$tags $c/$VERSION"
  written="$tags"
done

# The go.mod and go.sum from before the release commit: make release
# pointed the command's first-party requires at VERSION there already,
# with the replaces and the comment that says why they are there. The
# tidy is there in case the proxy's tidy above settled a third-party
# requirement differently; with the replaces back it answers from the
# tree, which is what was just tagged.
for c in "$@"; do
  git checkout -q "$START" -- "$c/go.mod" "$c/go.sum"
  "$HERE/versions.sh" set "$VERSION" "$c"
  (cd "$c" && GOWORK=off GOFLAGS=-mod=mod go mod tidy)
done
"$HERE/check-replaces.sh" "$@"
for c in "$@"; do git add -- "$c/go.mod" "$c/go.sum"; done
git commit -q -m "Point $* at the tree again after $VERSION"

# shellcheck disable=SC2086
git push origin --atomic HEAD $tags
