module github.com/ChristopherDavenport/agenteval/harbor

go 1.25.0

require (
	github.com/BurntSushi/toml v1.6.0
	github.com/ChristopherDavenport/agenteval v0.0.1
)

require (
	github.com/ChristopherDavenport/agentsession v0.0.5 // indirect
	github.com/ChristopherDavenport/agenttool v0.0.5 // indirect
	github.com/ChristopherDavenport/agentturn v0.0.6 // indirect
	github.com/ChristopherDavenport/agentturn/session v0.0.6 // indirect
	github.com/ChristopherDavenport/openresponses v0.0.9 // indirect
)

// The require names the released root a consumer fetches; the replace
// builds against the tree.
replace github.com/ChristopherDavenport/agenteval => ../
