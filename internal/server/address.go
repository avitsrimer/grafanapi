package server

import (
	"net"
	"strings"
)

// IsLoopbackAddress reports whether a --address value only accepts connections from this machine.
func IsLoopbackAddress(address string) bool {
	if strings.EqualFold(address, "localhost") {
		return true
	}

	ip := net.ParseIP(address)

	return ip != nil && ip.IsLoopback()
}
