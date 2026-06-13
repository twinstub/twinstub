package webhook

import (
	"fmt"
	"net"
	"syscall"
)

// dialGuard rejects connections to private address space when
// --allow-private-targets=false. The check runs at dial time on the
// resolved IP, which also covers DNS rebinding: whatever the resolver
// returned is what gets checked.
func dialGuard(allowPrivate bool) func(network, address string, c syscall.RawConn) error {
	return func(network, address string, _ syscall.RawConn) error {
		if allowPrivate {
			return nil
		}
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		ip := net.ParseIP(host)
		if ip == nil {
			return fmt.Errorf("webhook target %q did not resolve to an IP", host)
		}
		if isForbidden(ip) {
			return fmt.Errorf("webhook delivery to private address %s is blocked (run with --allow-private-targets to permit it)", ip)
		}
		return nil
	}
}

func isForbidden(ip net.IP) bool {
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() || // covers 169.254.169.254 metadata endpoints
		ip.IsLinkLocalMulticast() ||
		ip.IsUnspecified() ||
		ip.IsMulticast()
}
