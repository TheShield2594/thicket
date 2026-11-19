// Package thicket holds files from the repository root that the binary
// carries with it.
package thicket

import _ "embed"

// Changelog is CHANGELOG.md as of this build. `thicket changelog` prints
// from it, and `thicket update` shows what changed between two versions.
//
//go:embed CHANGELOG.md
var Changelog string
