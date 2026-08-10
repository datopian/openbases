// Package version exposes build identity. Values are injected at link time by the
// release pipeline so that every running binary can be traced to an exact commit
// and an immutable image digest (plan section 16.2).
package version

import "fmt"

var (
	// Version is the release tag, e.g. v0.3.1. "dev" for local builds.
	Version = "dev"
	// Commit is the exact git commit the binary was built from.
	Commit = "unknown"
	// BuildDate is the RFC3339 build timestamp.
	BuildDate = "unknown"
	// ImageDigest is the immutable container image digest, when built as an image.
	ImageDigest = "unknown"
)

// String renders the full build identity for logs, /health, and audit records.
func String() string {
	return fmt.Sprintf("version=%s commit=%s built=%s image=%s",
		Version, Commit, BuildDate, ImageDigest)
}
