module github.com/ChristopherDavenport/agenteval/harbor

go 1.25.0

require (
	github.com/BurntSushi/toml v1.6.0
	github.com/ChristopherDavenport/agenteval v0.0.7
	github.com/ChristopherDavenport/agentsession v0.0.15
)

require (
	github.com/ChristopherDavenport/agenttool v0.0.11 // indirect
	github.com/ChristopherDavenport/agentturn v0.0.12 // indirect
	github.com/ChristopherDavenport/agentturn/session v0.0.12 // indirect
	github.com/ChristopherDavenport/openresponses v0.0.12 // indirect
)

// The require names the released root a consumer fetches; the replace
// builds against the tree. A consumer ignores the replace, and make
// extracted, which CI runs on every pull request and on main, copies
// this module out of the tree, drops the replace and builds, vets and
// tests it against the root the require names. release-guard proves
// that root is the commit being tagged.
replace github.com/ChristopherDavenport/agenteval => ../
