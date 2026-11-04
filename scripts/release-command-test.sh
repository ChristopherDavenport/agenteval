#!/usr/bin/env bash
# Tests of scripts/release-command.sh's refusals, against throwaway
# repositories and a file proxy: each refusal must come before anything
# is changed, the tree clean and HEAD where it was. Then the release
# guard's checks of a command tag, which release-command runs before it
# writes one.
#
#   scripts/release-command-test.sh
set -u
HERE="$(cd "$(dirname "$0")" && pwd)"
# Every commit and annotated tag here, the script under test's among
# them, needs an identity, and a CI runner has none configured.
export GIT_AUTHOR_NAME=t GIT_AUTHOR_EMAIL=t@t GIT_COMMITTER_NAME=t GIT_COMMITTER_EMAIL=t@t
M=example.com/m
fails=0

# refuse <name> <setup: tagged|unserved-lib|behind-origin|no-test-flag|path-origin|branch> <what it says>
refuse() {
  name="$1"; setup="$2"; says="$3"
  d="$(mktemp -d)"
  git init -q --bare "$d/origin.git"
  git init -q -b main "$d/work"
  mkdir -p "$d/proxy/$M/@v" "$d/proxy/$M/lib/@v"
  serve() {
    echo v0.0.2 > "$d/proxy/$1/@v/list"
    printf '{"Version":"v0.0.2","Time":"2026-01-01T00:00:00Z"}\n' > "$d/proxy/$1/@v/v0.0.2.info"
    printf 'module %s\n\ngo 1.25\n' "$1" > "$d/proxy/$1/@v/v0.0.2.mod"
  }
  serve "$M"
  if [ "$setup" = unserved-lib ]; then : > "$d/proxy/$M/lib/@v/list"; else serve "$M/lib"; fi
  (
    cd "$d/work" || exit 1
    mkdir -p scripts lib cmd/x
    cp "$HERE/release-command.sh" "$HERE/versions.sh" "$HERE/release-guard.sh" "$HERE/release-notes.sh" "$HERE/check-extracted.sh" "$HERE/check-replaces.sh" scripts/
    printf 'module %s\n\ngo 1.25\n' "$M" > go.mod
    printf 'module %s/lib\n\ngo 1.25\n' "$M" > lib/go.mod
    printf 'module %s/cmd/x\n\ngo 1.25\n\nrequire %s v0.0.2\n\nreplace %s => ../..\n' "$M" "$M" "$M" > cmd/x/go.mod
    printf '# Changelog\n\n## v0.0.2 - 2026-01-01\n\n- x\n' > CHANGELOG.md
    git add . && git -c user.email=t@t -c user.name=t commit -qm init
    git tag v0.0.2 && git tag lib/v0.0.2
    if [ "$setup" = path-origin ]; then
      git remote add origin "$d/origin.git"
    else
      git remote add origin "file://$d/origin.git"
    fi
    git push -q origin main --tags 2>/dev/null
    case "$setup" in
      tagged) git tag cmd/x/v0.0.2 ;;
      behind-origin)
        git -c user.email=t@t -c user.name=t commit -q --allow-empty -m later
        git push -q origin main 2>/dev/null
        git reset -q --hard HEAD~1 ;;
      branch) git checkout -q -b topic ;;
    esac
    flag=1; [ "$setup" = no-test-flag ] && flag=
    before="$(git rev-parse HEAD)"
    GOMODCACHE="$d/modcache" GOFLAGS=-modcacherw RELEASE_COMMAND_TEST="$flag" RELEASE_PROXY="file://$d/proxy" GONOSUMDB="$M" GOSUMDB=off GOWORK=off \
      bash scripts/release-command.sh v0.0.2 cmd/x >/dev/null 2>"$d/err" && exit 10
    [ "$(git rev-parse HEAD)" = "$before" ] || exit 11
    [ -z "$(git status --porcelain)" ] || exit 12
    grep -qF -- "$says" "$d/err" || exit 13
    exit 0
  )
  rc=$?
  chmod -R u+w "$d" 2>/dev/null; rm -rf "$d"
  if [ "$rc" != 0 ]; then
    echo "FAIL: release-command $name: rc $rc (10 ran through, 11 committed, 12 left the tree dirty, 13 refused for another reason)"
    fails=$((fails + 1))
  fi
}

refuse "a command tag that exists" tagged "cmd/x/v0.0.2 is tagged already"
refuse "a library the proxy does not serve" unserved-lib "$M/lib@v0.0.2 is not on the proxy"
refuse "HEAD behind origin/main" behind-origin "does not contain origin/main"
refuse "RELEASE_PROXY outside a test" no-test-flag "RELEASE_PROXY"
refuse "RELEASE_PROXY with an origin not file://" path-origin "RELEASE_PROXY"
refuse "a branch other than main" branch "not on main"

# The whole second step, on a release the root and lib were tagged at:
# the command is tagged from a commit whose go.mod holds no replace and
# was tidied against the proxy, and the commit after it puts back the
# go.mod and go.sum from before, comment and replaces included, so the
# tree builds the command against itself again. Branch and tag reach
# origin. The proxy serves real module zips, written with python3.
release_ok() {
  d="$(mktemp -d)"
  git init -q --bare "$d/origin.git"
  git init -q -b main "$d/work"
  (
    cd "$d/work" || exit 1
    mkdir -p scripts lib cmd/x
    cp "$HERE/release-command.sh" "$HERE/versions.sh" "$HERE/release-guard.sh" "$HERE/release-notes.sh" "$HERE/check-extracted.sh" "$HERE/check-replaces.sh" scripts/
    printf 'module %s\n\ngo 1.25\n' "$M" > go.mod
    printf 'package m\n\nfunc Root() int { return 1 }\n' > m.go
    printf 'module %s/lib\n\ngo 1.25\n' "$M" > lib/go.mod
    printf 'package lib\n\nfunc Lib() int { return 2 }\n' > lib/lib.go
    printf 'module %s/cmd/x\n\ngo 1.25\n\nrequire (\n\t%s v0.0.2\n\t%s/lib v0.0.2\n)\n\n// Why the replaces are here.\nreplace (\n\t%s => ../..\n\t%s/lib => ../../lib\n)\n' "$M" "$M" "$M" "$M" "$M" > cmd/x/go.mod
    : > cmd/x/go.sum
    printf 'package main\n\nimport (\n\t"fmt"\n\n\tm "%s"\n\t"%s/lib"\n)\n\nfunc main() { fmt.Println(m.Root() + lib.Lib()) }\n' "$M" "$M" > cmd/x/main.go
    printf '# Changelog\n\n## v0.0.2 - 2026-01-01\n\n- x\n' > CHANGELOG.md
    git add . && git -c user.email=t@t -c user.name=t commit -qm "Release v0.0.2"
    git tag -a v0.0.2 -m v0.0.2 && git tag -a lib/v0.0.2 -m v0.0.2
    git remote add origin "file://$d/origin.git"
    git push -q origin main --tags 2>/dev/null
    # The proxy serves what the tags name.
    for p in "$M:." "$M/lib:lib"; do
      mod="${p%%:*}"; dir="${p#*:}"
      mkdir -p "$d/proxy/$mod/@v"
      echo v0.0.2 > "$d/proxy/$mod/@v/list"
      printf '{"Version":"v0.0.2","Time":"2026-01-01T00:00:00Z"}\n' > "$d/proxy/$mod/@v/v0.0.2.info"
      cp "$dir/go.mod" "$d/proxy/$mod/@v/v0.0.2.mod"
      python3 -I -c '
import sys, zipfile
mod, out, files = sys.argv[1], sys.argv[2], sys.argv[3:]
with zipfile.ZipFile(out, "w") as z:
    for f in files:
        z.write(f.split("=")[0], mod + "@v0.0.2/" + f.split("=")[1])
' "$mod" "$d/proxy/$mod/@v/v0.0.2.zip" "$dir/go.mod=go.mod" "$dir/$( [ "$dir" = . ] && echo m.go || echo lib.go )=$( [ "$dir" = . ] && echo m.go || echo lib.go )" || exit 1
    done
    start="$(git rev-parse HEAD)"
    GOMODCACHE="$d/modcache" GOFLAGS=-modcacherw RELEASE_COMMAND_TEST=1 RELEASE_PROXY="file://$d/proxy" GONOSUMDB="$M" GOSUMDB=off GOWORK=off \
      bash scripts/release-command.sh v0.0.2 cmd/x >"$d/out" 2>&1 || { cat "$d/out"; exit 10; }
    tagged="$(git rev-parse cmd/x/v0.0.2^{commit})" || exit 11
    [ "$tagged" = "$(git rev-parse HEAD~1)" ] || exit 11
    ! git show "$tagged:cmd/x/go.mod" | grep -q replace || exit 12
    git show "$tagged:cmd/x/go.sum" | grep -qF "$M/lib v0.0.2 h1:" || exit 12
    [ -z "$(git ls-tree -r --name-only "$tagged" -- cmd/x | grep -vxE 'cmd/x/(go\.mod|go\.sum|main\.go)')" ] || exit 12  # a file beside go.mod and go.sum, such as a built binary
    git diff --quiet "$start" HEAD -- cmd/x || exit 13
    [ "$(git ls-remote origin refs/heads/main | cut -f1)" = "$(git rev-parse HEAD)" ] || exit 14
    git ls-remote --tags origin | grep -q 'refs/tags/cmd/x/v0.0.2$' || exit 14
    [ -z "$(git status --porcelain)" ] || exit 15
    exit 0
  )
  rc=$?
  chmod -R u+w "$d" 2>/dev/null; rm -rf "$d"
  if [ "$rc" != 0 ]; then
    echo "FAIL: release-command on a published release: rc $rc (10 refused, 11 tag not on the release commit, 12 tagged go.mod has a replace or was not tidied against the proxy, 13 the tree's command differs from before, 14 not pushed, 15 tree dirty)"
    fails=$((fails + 1))
  fi
}

release_ok

# command <accept|refuse> <tag> <version cmd/x requires the root and lib at>
#         <replace: yes|no> <what the proxy serves at the tag's version: all|root|none>
#         [published tags...]
# A command module at cmd/x, released in the second step: the root and
# its library lib were tagged before, at an earlier commit, and are on
# the proxy; the command's go.mod drops its replaces and requires both
# at the version released. The guard refuses a command with any
# replace, since go install refuses it, and one requiring a first-party
# module the proxy does not serve yet. A file proxy in the case's
# directory stands in for proxy.golang.org.
command() {
  want="$1"; tag="$2"; req="$3"; repl="$4"; served="$5"; shift 5
  d="$(mktemp -d)"
  git init -q --bare "$d/origin.git"
  git init -q "$d/work"
  v="v${tag##*/v}"
  mkdir -p "$d/proxy/$M/@v" "$d/proxy/$M/lib/@v"
  serve() {
    echo "$v" > "$d/proxy/$1/@v/list"
    printf '{"Version":"%s","Time":"2026-01-01T00:00:00Z"}\n' "$v" > "$d/proxy/$1/@v/$v.info"
    printf 'module %s\n\ngo 1.25\n' "$1" > "$d/proxy/$1/@v/$v.mod"
  }
  case "$served" in
    all) serve "$M"; serve "$M/lib" ;;
    root) serve "$M"; : > "$d/proxy/$M/lib/@v/list" ;;
    *) : > "$d/proxy/$M/@v/list"; : > "$d/proxy/$M/lib/@v/list" ;;
  esac
  (
    cd "$d/work" || exit 1
    mkdir -p lib cmd/x
    printf 'module %s\n\ngo 1.25\n' "$M" > go.mod
    printf 'module %s/lib\n\ngo 1.25\n' "$M" > lib/go.mod
    git add . && git -c user.email=t@t -c user.name=t commit -qm init
    git tag "$v" && git tag "lib/$v"
    git remote add origin "$d/origin.git"
    for t in "$@"; do git tag "$t"; done
    git push -q origin --tags 2>/dev/null
    printf 'module %s/cmd/x\n\ngo 1.25\n\nrequire (\n\t%s %s\n\t%s/lib %s\n)\n' "$M" "$M" "$req" "$M" "$req" > cmd/x/go.mod
    [ "$repl" = no ] || printf '\nreplace %s => ../..\n' "$M" >> cmd/x/go.mod
    git add . && git -c user.email=t@t -c user.name=t commit -qm "release the command"
    # A module cache of its own: one the case shared would serve the
    # version to the cases after it.
    GOMODCACHE="$d/modcache" GOPROXY="file://$d/proxy" GONOSUMDB="$M" GOFLAGS="-mod=mod -modcacherw" GOWORK=off \
      bash "$HERE/release-guard.sh" "$tag" >/dev/null 2>&1
  )
  rc=$?
  chmod -R u+w "$d" 2>/dev/null; rm -rf "$d"
  got=refuse; [ "$rc" = 0 ] && got=accept
  if [ "$got" != "$want" ]; then
    echo "FAIL: command guard $tag (requires $req, replace $repl, served $served) with [$*]: want $want, got $got"
    fails=$((fails + 1))
  fi
}

command accept cmd/x/v0.0.2 v0.0.2 no all v0.0.1
command refuse cmd/x/v0.0.2 v0.0.2 yes all v0.0.1
command refuse cmd/x/v0.0.2 v0.0.2 no none v0.0.1
command refuse cmd/x/v0.0.2 v0.0.2 no root v0.0.1
command refuse cmd/x/v0.0.2 v0.0.1 no all v0.0.1
command refuse cmd/x/v0.0.2 v0.0.2 no all v0.0.1 cmd/x/v0.0.2
command refuse cmd/x/v0.0.2 v0.0.2 no all v0.0.1 cmd/x/v0.0.3

[ "$fails" = 0 ] && echo "release-command tests passed"
exit "$fails"
