package core

import (
	"net"
	"strings"
)

// ValidEndpointHost accepts unicast IPv4 addresses and DNS names usable as SSH
// destinations without options, quoting or resolver search-path syntax.
func ValidEndpointHost(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return ip.To4() != nil && !ip.IsUnspecified() && !ip.IsMulticast()
	}
	if host == "" || len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}
