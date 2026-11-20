package client

// DefaultSocketPathOn and UserSocketPathOn are the socket defaults as they
// are on goos, so the macOS fallback is specified on any host.
var (
	DefaultSocketPathOn = defaultSocketPath
	UserSocketPathOn    = userSocketPath
)
