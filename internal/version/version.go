// Package version holds build metadata injected via ldflags.
package version

var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// UserAgent is sent on every outgoing webhook request.
func UserAgent() string {
	return "twinstub/" + Version
}
