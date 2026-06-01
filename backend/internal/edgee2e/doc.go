// Package edgee2e holds the cross-plane end-to-end and integration tests that
// exercise the ccdirect data-plane and the cchub control-plane together. It lives
// in its own package (rather than inside ccdirect or cchub) because importing both
// sides from either one would create a cycle; as an external test package it uses
// only their exported APIs plus the shared contract package.
//
// All files here are build-tagged `unit`; this doc file is untagged so tooling
// (go build, golangci-lint) always sees a buildable file in the package.
package edgee2e
