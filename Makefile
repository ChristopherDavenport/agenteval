GO ?= go
STATICCHECK ?= $(GO) run honnef.co/go/tools/cmd/staticcheck@latest
GOVULNCHECK ?= $(GO) run golang.org/x/vuln/cmd/govulncheck@latest
# Nested modules that are tested alongside the library but keep their own
# dependencies out of it. harbor is a library, released with the root.
# cmd/agenteval is the command, which requires harbor and the root; it is
# released in a second step (release-command), with its replaces
# dropped, because go install refuses a module that holds any.
LIBRARIES = harbor
COMMANDS = cmd/agenteval
SUBMODULES = $(LIBRARIES) $(COMMANDS)

.PHONY: build deps replaces test vet fmt tidy tidy-check lint vuln check \
	extracted guard-test release-guard release release-command clean

build:
	$(GO) build ./...
	@for m in $(SUBMODULES); do (cd $$m && $(GO) build ./...) || exit 1; done

# The root module runs the loop to produce sessions and must build from
# the four siblings and the standard library alone; anything that needs
# another dependency, such as harbor's TOML decoder, is a nested module.
deps:
	@deps=$$($(GO) list -deps -f '{{if not .Standard}}{{.ImportPath}}{{end}}' ./... | grep -v '^github.com/ChristopherDavenport/agenteval' | grep -v '^github.com/ChristopherDavenport/agentsession' | grep -v '^github.com/ChristopherDavenport/agenttool' | grep -v '^github.com/ChristopherDavenport/agentturn' | grep -v '^github.com/ChristopherDavenport/openresponses' || true); \
	  test -z "$$deps" || { echo "root module depends on: $$deps"; exit 1; }

# Every first-party module a nested module requires must also be
# replaced, at a path that exists. There is no go.work in this
# repository: the replace is the only thing that builds the tree against
# itself, and it is load-bearing at release time too.
#
# Every module is released at one version, from one commit, and requires
# its siblings at exactly that version — a version the proxy cannot serve
# until the tag is pushed. The replace is what lets the release commit
# resolve, tidy and build. Lose one and the next release fails at make
# tidy, or silently pins that module to the previous release.
#
# A replace is a property of the main module, so consumers ignore it and
# get the require. That is safe only because the require names the commit
# the module is tagged from; release-guard is what proves it.
replaces:
	@scripts/check-replaces.sh $(SUBMODULES)

test:
	$(GO) test -race ./...
	@for m in $(SUBMODULES); do (cd $$m && $(GO) test -race ./...) || exit 1; done

vet:
	$(GO) vet ./...
	@for m in $(SUBMODULES); do (cd $$m && $(GO) vet ./...) || exit 1; done

tidy:
	$(GO) mod tidy
	@for m in $(SUBMODULES); do (cd $$m && $(GO) mod tidy) || exit 1; done

# Fails when go mod tidy would change any go.mod or go.sum, without
# writing, so a stray dependency shows up in make check and not only in
# CI's diff.
tidy-check:
	$(GO) mod tidy -diff
	@for m in $(SUBMODULES); do (cd $$m && $(GO) mod tidy -diff) || exit 1; done

fmt:
	gofmt -l . && test -z "$$(gofmt -l .)"

lint:
	$(STATICCHECK) ./...
	@for m in $(SUBMODULES); do (cd $$m && $(STATICCHECK) ./...) || exit 1; done

vuln:
	$(GOVULNCHECK) ./...
	@for m in $(SUBMODULES); do (cd $$m && $(GOVULNCHECK) ./...) || exit 1; done

# Everything CI runs, bar extracted below.
check: fmt tidy-check vet deps replaces lint vuln test guard-test

# Builds, vets and tests each nested module the way a consumer gets it:
# extracted to
# a directory with no parent go.mod, with the in-tree replaces dropped,
# so the require lines are answered by the proxy. replaces above checks
# that a require is present and release-guard checks that it names the
# version being tagged; both are claims about a version string, and
# neither compiles anything against it. Point agenttool's mcpclient at
# agenttool v0.0.1 and both stay silent while this fails with
# "undefined: agenttool.WithResource".
#
# release-guard.sh had this once, at openresponses v0.0.11: "build it the
# way a consumer does", running build, vet and test. Introducing the
# replace at v0.0.12 turned that line into a build against the tree,
# because GOWORK=off stopped meaning "no local root", and it went on
# printing ok. release-guard.sh now calls this script in its place.
#
# Needs the network, so it is not part of check. It could not be anyway:
# release points every require at the version being released, and the
# proxy cannot serve that until the tag is pushed. CI runs it on pull
# requests and on main.
#
# A module is skipped while the tree has moved on from the release a
# first-party require names (see check-extracted.sh): the command, which
# requires the root and harbor, until make release publishes them at the
# version it is then pointed at.
extracted:
	@scripts/check-extracted.sh $(SUBMODULES)

# The release-command script's own tests, and the release guard's checks
# of a command tag, against throwaway repositories and a file proxy.
guard-test:
	@scripts/release-command-test.sh

# The module path of the root, which every first-party require and
# replace is written against.
MODULE := $(shell $(GO) list -m)

# Checks one tag is safe to push, before it is pushed. A pushed tag is
# permanent — the proxy and the checksum database keep the version
# forever — so this is the last point at which a mistake is free:
#   make release-guard TAG=harbor/v0.1.0
release-guard:
	@test -n "$(TAG)" || { echo "usage: make release-guard TAG=<tag>"; exit 1; }
	@scripts/release-guard.sh "$(TAG)"

# Every tag the first step of a release writes: the root and one per
# library, all at the same version, all from the one commit below. The
# command is released in a second step (release-command), once these are
# on the proxy.
RELEASE_TAGS = $(VERSION) $(patsubst %,%/$(VERSION),$(LIBRARIES))

# Cut a release:
#
#   make release VERSION=v0.1.0
#
# The root and every library are released at one version, from one
# commit, and require their first-party siblings at exactly that version.
# So the first thing this does is point every nested module, the command
# among them, at VERSION — a version that does not exist yet. That
# resolves because each nested go.mod replaces its first-party
# requirements with the tree (see replaces above); tidy, build and test
# all see the code being tagged. The command is not tagged here: its tag
# must hold no replace, and it cannot drop them until the proxy serves
# this version (release-command, below).
#
# --atomic lands every ref in one transaction, so no window exists in
# which one tag is visible without the others, and none in which a
# published go.mod names a version the proxy cannot serve.
#
# The root is guarded and tagged first, then each library, because a
# library's guard proves the root tag of that version names this
# commit — which it cannot do before that tag exists. Every tag is local
# until the push; if a guard refuses, undo with git reset --hard HEAD~1
# and git tag -d the tags written.
#
# TRAILER, when set, is appended to the commit message.
#
# The changelog is dated through a temp file rather than sed -i, which is
# a GNU-ism: BSD sed reads the argument after -i as a backup suffix, so
# the GNU spelling fails outright on macOS, where these releases are cut.
# The temp file is removed if sed dies, so a failed run leaves nothing
# untracked behind for the clean-tree gate to trip over next time.
release:
	@test -n "$(VERSION)" || { echo "usage: make release VERSION=vX.Y.Z"; exit 1; }
	@test $(words $(RELEASE_TAGS)) -le 3 || { \
	  echo "$(words $(RELEASE_TAGS)) tags would be pushed at once, and GitHub creates no events"; \
	  echo "for a push of more than three tags — every tag would land and the release"; \
	  echo "workflow would silently never run. Either push the tags one at a time and"; \
	  echo "lose the atomic push, or create the GitHub releases from here with gh."; \
	  exit 1; }
	@test "$(origin SUBMODULES)" = file || { echo "do not override SUBMODULES here: a command-line override propagates into the bump, tidy and check below, so a module would be tagged having checked a subset."; exit 1; }
	@grep -q '^## Unreleased$$' CHANGELOG.md || { echo "CHANGELOG.md has no Unreleased section"; exit 1; }
	@test -z "$$(git status --porcelain)" || { echo "working tree is not clean"; exit 1; }
	@scripts/versions.sh set $(VERSION) $(SUBMODULES)
	sed 's/^## Unreleased$$/## $(VERSION) - '"$$(date +%F)"'/' CHANGELOG.md > CHANGELOG.md.tmp \
	  && mv CHANGELOG.md.tmp CHANGELOG.md \
	  || { rm -f CHANGELOG.md.tmp; exit 1; }
	$(MAKE) tidy
	$(MAKE) check
	@scripts/versions.sh check $(VERSION) $(SUBMODULES)
	git add -A && git commit -q -m "Release $(VERSION)" $(if $(TRAILER),-m "$(TRAILER)")
	@scripts/release-guard.sh "$(VERSION)"
	@notes="$$(scripts/release-notes.sh $(VERSION))" || exit 1; \
	 git tag -a $(VERSION) -m "$$notes"
	@set -e; notes="$$(scripts/release-notes.sh $(VERSION))"; \
	for m in $(LIBRARIES); do \
	  scripts/release-guard.sh "$$m/$(VERSION)"; \
	  git tag -a $$m/$(VERSION) -m "$$notes"; \
	done
	git push origin --atomic HEAD $(RELEASE_TAGS)

# The second step: once the proxy serves the root and the libraries at
# VERSION, release the command with its replaces dropped and tidied
# against the proxy, tagged cmd/agenteval/VERSION, then point it at the
# tree again; scripts/release-command.sh says how.
#   make release-command VERSION=v0.1.0
release-command:
	@test -n "$(VERSION)" || { echo "usage: make release-command VERSION=vX.Y.Z"; exit 1; }
	@scripts/release-command.sh $(VERSION) $(COMMANDS)

clean:
	rm -rf .cache
