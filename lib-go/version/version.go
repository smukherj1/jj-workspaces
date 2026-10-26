// Package version holds the project version, set at build time by the Taskfile with
// -ldflags "-X github.com/smukherj1/jj-workspaces/lib-go/version.Version=<version>".
package version

// Version is the project version, or "dev" for builds without -ldflags.
var Version = "dev"
