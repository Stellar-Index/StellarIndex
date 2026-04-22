// Package version exposes build-time version metadata injected
// via -ldflags at build time. See Makefile `LD_FLAGS`.
//
// Values default to "dev" for unbuilt / test runs.
package version

var (
	// Version is the git describe --tags --always --dirty output,
	// or "dev" for local unbuilt runs.
	Version = "dev"

	// BuildDate is the ISO-8601 UTC timestamp at build time.
	BuildDate = "unknown"
)

// String returns a human-readable one-line summary.
func String() string {
	return Version + " (" + BuildDate + ")"
}
