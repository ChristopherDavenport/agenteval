module github.com/ChristopherDavenport/agenteval/cmd/agenteval

go 1.25.0

require (
	github.com/ChristopherDavenport/agenteval v0.0.11
	github.com/ChristopherDavenport/agenteval/harbor v0.0.11
	github.com/ChristopherDavenport/agentsession v0.0.34
	github.com/ChristopherDavenport/openresponses v0.0.18
)

require (
	github.com/BurntSushi/toml v1.6.0 // indirect
	github.com/ChristopherDavenport/agenttool v0.0.28 // indirect
	github.com/ChristopherDavenport/agentturn v0.0.35 // indirect
	github.com/ChristopherDavenport/agentturn/session v0.0.35 // indirect
)

// The requires name the released modules a consumer fetches; the
// replaces build against the tree. A consumer ignores them, and make
// extracted copies this module out of the tree, drops them and builds
// it against the releases the requires name. release-guard proves
// those are the commit being tagged.
replace (
	github.com/ChristopherDavenport/agenteval => ../../
	github.com/ChristopherDavenport/agenteval/harbor => ../../harbor
)
