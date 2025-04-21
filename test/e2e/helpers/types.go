package helpers

// pingCommand is the type to hold ping command.
type pingCommand string

const (
	// IPv4PingCommand is a ping command for IPv4.
	IPv4PingCommand pingCommand = "ping"
	// IPv6PingCommand is a ping command for IPv6.
	IPv6PingCommand pingCommand = "ping6"
)
