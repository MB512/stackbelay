// Package buildinfo holds the version of the binaries.
package buildinfo

// Version is set at build time, e.g.
//
//	go build -ldflags "-X github.com/MB512/stackvault/internal/buildinfo.Version=v0.1.0"
//
// (like setting <Version> in a .csproj). It is only written by the linker,
// never at run time.
var Version = "dev"
