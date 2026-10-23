// Package version holds build metadata, set with -ldflags at release time.
package version

var (
	// Version is the CLI version without a leading "v".
	Version = "dev"
	// Commit is the source revision.
	Commit = "none"
	// Date is the build date.
	Date = "unknown"
)
