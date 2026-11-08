module github.com/ChristopherDavenport/agenteval/cmd/agenteval

go 1.25.0

require (
	github.com/ChristopherDavenport/agenteval v0.0.14
	github.com/ChristopherDavenport/agenteval/harbor v0.0.14
	github.com/ChristopherDavenport/agentsession v0.0.37
	github.com/ChristopherDavenport/openresponses v0.0.18
)

require (
	github.com/BurntSushi/toml v1.6.0 // indirect
	github.com/ChristopherDavenport/agenttool v0.0.29 // indirect
	github.com/ChristopherDavenport/agentturn v0.0.37 // indirect
	github.com/ChristopherDavenport/agentturn/session v0.0.37 // indirect
)

// The replaces build the command against the tree. go install refuses
// a module holding any, so make release-command releases it with them
// dropped, tidied against the root and harbor releases the requires
// name, and puts them back after the tag. make extracted copies this
// module out of the tree, drops them and builds it against those
// releases.
replace (
	github.com/ChristopherDavenport/agenteval => ../../
	github.com/ChristopherDavenport/agenteval/harbor => ../../harbor
)
