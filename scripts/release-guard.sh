#!/usr/bin/env bash
# Checks a tag is safe to push before it is pushed. A pushed tag is
# permanent: the module proxy and the checksum database keep the version
# forever, so a mistake can only be retracted, never withdrawn.
#
#   scripts/release-guard.sh v0.0.7
#   scripts/release-guard.sh harbor/v0.0.7
#   scripts/release-guard.sh cmd/agenteval/v0.0.7
#
# Exits non-zero, with the reason, if the tag would publish a version
# that sorts below one already released, or a module whose go.mod does
# not name the very commit it is tagged from.
#
# A nested tag is only checkable once the root tag of the same version
# exists locally, so run this in the order `make release` does: guard the
# root, tag it, then guard and tag each nested module. Nothing is public
# until the push.
#
# A command's tag, cmd/<name>/vX.Y.Z, is the second step of a release
# (make release-command): the root and its libraries were tagged at that
# version from an earlier commit and are on the proxy, and the command's
# go.mod requires them there with no replace, since go install refuses a
# module that holds one.

set -euo pipefail

TAG="${1:-}"

die() { echo "release-guard: $*" >&2; exit 1; }
ok()  { echo "  ok  $*"; }

# The scripts beside this one; the release scripts' tests run it from a
# throwaway repository.
HERE="$(cd "$(dirname "$0")" && pwd)"

[ -n "$TAG" ] || die "usage: $0 <tag>"

# Newest version wins a version sort; -V orders v0.0.9 before v0.0.10.
newest() { sort -V | tail -1; }

echo "release-guard: $TAG"

# --- Repository state -------------------------------------------------
[ -z "$(git status --porcelain)" ] || die "working tree is dirty; commit or stash first"
ok "working tree clean"

# --- What origin has published ----------------------------------------
# The version floor has to come from what is published, not from what
# this checkout happens to know. A clone that has not fetched recently
# carries a stale floor, and a version that sorts below one already on
# the proxy is the one mistake with no remedy: proxy.golang.org and
# sum.golang.org serve both forever and nobody can supersede the older
# content. Reading only local tags made this script approve exactly that.
#
# git ls-remote is read-only, so unlike a git fetch --tags at the top of
# a check it does not mutate the caller's tag state as a side effect.
#
# It fails closed. If origin cannot be reached then the push could not
# have succeeded either, so refusing costs a release nothing, while a
# silent fallback to local tags would reinstate the stale floor on
# precisely the day the network is unreliable.
REMOTE_LS="$(git ls-remote --tags origin 2>&1)" \
  || die "cannot read the published tags from origin:
            $REMOTE_LS
            The floor is what origin has published, so there is no safe answer
            without it, and a push could not have succeeded either."

# ls-remote returns the peeled ^{} refs alongside the tags; drop them.
REMOTE_TAGS="$(printf '%s\n' "$REMOTE_LS" | sed -e 's|.*refs/tags/||' -e '/\^{}$/d')"

# The floor is the union of local and remote. Remote alone would break
# make release: it writes the root tag locally and does not push until
# the end, so the checks below still have to see local tags.
ALL_TAGS="$( { git tag -l; printf '%s\n' "$REMOTE_TAGS"; } | sort -u )"

# Tags matching a pattern, from that union. grep exits 1 on no match,
# which errexit would take as a failure, so the empty case is explicit.
matching() { printf '%s\n' "$ALL_TAGS" | grep -E "$1" || true; }
ok "read the published tags from origin"

git rev-parse -q --verify "refs/tags/$TAG" >/dev/null \
  && die "tag $TAG already exists locally"
printf '%s\n' "$REMOTE_TAGS" | grep -qxF -- "$TAG" \
  && die "tag $TAG already exists on origin"
ok "tag is new"

# --- Root version already released ------------------------------------
# Nested tags are <dir>/vX.Y.Z, so 'v*' matches root tags only.
ROOT_LATEST="$(matching '^v' | newest)"
[ -n "$ROOT_LATEST" ] || die "no root tag found; cannot establish the version floor"

case "$TAG" in
  v*)
    # Root tag: must move forward.
    [ "$(printf '%s\n%s\n' "$ROOT_LATEST" "$TAG" | newest)" = "$TAG" ] \
      || die "$TAG does not sort above the current root release $ROOT_LATEST"
    ok "$TAG is newer than $ROOT_LATEST"
    ;;

  cmd/*/v*)
    # A command, released in the second step (AGENTS.md, Releases): the
    # root and its libraries were tagged at this version before, and are
    # on the proxy; the command requires them there, with no replace,
    # since go install refuses a module that has one.
    DIR="${TAG%/v*}"                 # cmd/agenteval
    VERSION="v${TAG##*/v}"
    [ -f "$DIR/go.mod" ] || die "no module at $DIR (expected $DIR/go.mod)"
    printf '%s\n' "$VERSION" | grep -qE '^v[0-9]+\.[0-9]+\.[0-9]+$' \
      || die "$TAG is not a plain <dir>/vX.Y.Z version"
    ! grep -qE '^[[:space:]]*replace([[:space:]]|\()' "$DIR/go.mod" \
      || die "$DIR/go.mod holds a replace directive; go install $TAG would refuse it.
            Drop it and tidy against the proxy first (make release-command)."
    ok "$DIR holds no replace"
    DIR_LATEST="$(matching "^$DIR/v" | sed "s|^$DIR/||" | newest)"
    if [ -n "$DIR_LATEST" ]; then
      [ "$(printf '%s\n%s\n' "$DIR_LATEST" "$VERSION" | newest)" = "$VERSION" ] \
        || die "$VERSION does not sort above $DIR's current release $DIR_LATEST"
    fi
    ok "$VERSION is new for $DIR"
    git rev-parse -q --verify "refs/tags/$VERSION" >/dev/null \
      || die "the root is not tagged $VERSION; make release tags it first"
    git merge-base --is-ancestor "$VERSION" HEAD \
      || die "root $VERSION is not an ancestor of this commit"
    ok "root $VERSION was released before this commit"
    "$HERE/versions.sh" check "$VERSION" "$DIR" \
      || die "$DIR does not require its first-party modules at $VERSION"
    ok "requires every first-party module at $VERSION"
    # Every first-party module it requires, the root and each library
    # (here harbor), must be on the proxy: go install resolves them
    # there. Asked from outside any module: inside the root, the go
    # command answers for the main module from the tree, not the proxy.
    # And asked of the proxy alone, as go install asks: GOENV=off keeps a
    # GOPRIVATE in the go env file from sending it to origin (an empty
    # variable falls back to the file), and the default GOPROXY's
    # ",direct" would do the same. A GOPROXY set by the caller, such as
    # release-command's or its tests' file proxy, is kept.
    MODULE="$(sed -n 's/^module[[:space:]]*//p' go.mod | head -1)"
    for p in $(awk -v p="$MODULE" '
        /^require[[:space:]]*\(/ { blk = 1; next }
        blk && /^\)/             { blk = 0; next }
        blk && ($1 == p || index($1, p "/") == 1) { print $1; next }
        /^require[[:space:]]/ && ($2 == p || index($2, p "/") == 1) { print $2 }
      ' "$DIR/go.mod" | sort -u); do
      err="$(cd "$(mktemp -d)" && GOENV=off GOPRIVATE= GONOPROXY= GOPROXY="${GOPROXY:-https://proxy.golang.org}" GOWORK=off GOFLAGS= go list -m -e -f '{{with .Error}}{{.Err}}{{end}}' "$p@$VERSION" 2>&1)"
      [ -z "$err" ] || die "$p@$VERSION is not on the proxy yet: $err
            Release the command once the root and its libraries are served."
      ok "$p@$VERSION is on the proxy"
    done
    ;;

  */v*)
    DIR="${TAG%/v*}"                 # harbor
    VERSION="v${TAG##*/v}"           # v0.0.7
    [ -f "$DIR/go.mod" ] || die "no module at $DIR (expected $DIR/go.mod)"

    # Every published module shares one version line. A nested module
    # numbered below the newest root drags consumers' root module
    # backwards: they select the highest nested version available, and
    # its go.mod then pins the root, through minimal version selection.
    [ "$(printf '%s\n%s\n' "$ROOT_LATEST" "$VERSION" | newest)" = "$VERSION" ] \
      || die "$VERSION sorts below the current root release $ROOT_LATEST;
            a nested version under the root silently downgrades consumers.
            Tag $ROOT_LATEST or later."
    ok "$VERSION is at or above the root release $ROOT_LATEST"

    # And it has to move that module forward too, or the proxy keeps
    # serving the older content under a version nobody can supersede.
    DIR_LATEST="$(matching "^$DIR/v" | sed "s|^$DIR/||" | newest)"
    if [ -n "$DIR_LATEST" ]; then
      [ "$(printf '%s\n%s\n' "$DIR_LATEST" "$VERSION" | newest)" = "$VERSION" ] \
        || die "$VERSION does not sort above $DIR's current release $DIR_LATEST"
      ok "$VERSION is newer than $DIR/$DIR_LATEST"
    fi

    # The whole invariant, in two checks.
    #
    # First: every first-party version this module requires is the
    # version being tagged. A consumer who takes only this module gets
    # exactly those versions, so this is what makes the shared version
    # line mean something rather than being decoration.
    "$HERE/versions.sh" check "$VERSION" "$DIR" \
      || die "$DIR does not require its first-party siblings at $VERSION"
    ok "requires every first-party sibling at $VERSION"

    # Second: the root tag of that version names this very commit. The
    # require above claims this module was built against root $VERSION;
    # this is what makes the claim true rather than merely asserted. It
    # is why there is no build against a published root here — there is
    # no older root in the picture to drift from.
    ROOT_TAG_COMMIT="$(git rev-list -n1 "$VERSION" 2>/dev/null || true)"
    [ -n "$ROOT_TAG_COMMIT" ] \
      || die "$VERSION is not tagged; the root and its nested modules are tagged from one commit,
            so tag the root in the same run (make release does this)"
    [ "$ROOT_TAG_COMMIT" = "$(git rev-parse HEAD)" ] \
      || die "root $VERSION points at $ROOT_TAG_COMMIT, not HEAD;
            $DIR would claim to be built against a root it was not built against"
    ok "root $VERSION is this commit"

    # Build, vet and test it the way a consumer gets it: extracted, with
    # the replace dropped, so the require line above is resolved from the
    # proxy rather than from the directory next door.
    #
    # This line used to read (cd "$DIR" && GOWORK=off go build ./...),
    # described as proof the module "is buildable as published". It was
    # that, once. A nested go.mod carrying replace <root> => .. makes
    # GOWORK=off resolve the root from the tree, so it became a build of
    # the tree against itself — make build, run twice — while still
    # printing ok. check-extracted.sh restores what the line claimed.
    #
    # It can only run once the root version is on the proxy, and during
    # make release it is not: the root tag is written locally and pushed
    # on the last line. Nothing is lost by skipping it there. The two
    # checks immediately above have already established that every
    # first-party require names $VERSION and that root $VERSION is this
    # commit, so root $VERSION *is* this tree, and make check compiled
    # $DIR against this tree before the release commit was written. The
    # extracted build would re-derive that through the proxy. Where it
    # earns its keep is on a require naming an earlier release, which is
    # every run outside make release: CI on pull requests and on main,
    # and make release-guard on a tag whose root is already published.
    # go list -m reports every module in the workspace, so the root has
    # to be asked for outside it.
    MODULE="$(GOWORK=off go list -m)"
    if [ -z "$(GOWORK=off go list -m -e -f '{{with .Error}}{{.Err}}{{end}}' \
                 "$MODULE@$VERSION")" ]; then
      "$HERE/check-extracted.sh" "$DIR" \
        || die "$DIR does not build against $MODULE@$VERSION as a consumer gets it"
    else
      echo "  --  $MODULE@$VERSION is not on the proxy yet, so $DIR cannot be"
      echo "      built the way a consumer gets it. Root $VERSION is this commit"
      echo "      and make check compiled $DIR against it, so nothing is unproven;"
      echo "      the extracted build runs in CI once the tags are pushed."
    fi
    ;;

  *)
    die "unrecognised tag shape: $TAG (expected vX.Y.Z or <dir>/vX.Y.Z)"
    ;;
esac

echo "release-guard: $TAG is safe to push"
