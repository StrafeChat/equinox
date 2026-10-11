// Package safehttp is the outbound-request guard for every place the server fetches a URL
// that someone else chose - a link a user pasted (unfurl), a push endpoint a device
// registered (push). Such a request must never be steerable into this host's own network:
// the admin port of a database, a cloud metadata service, another container on the
// compose network. The check runs at dial time, after DNS resolution and immediately
// before the socket connects, so it also covers redirect targets and DNS rebinding, which
// a one-shot pre-flight lookup would miss.
package safehttp

import (
	"errors"
	"net"
	"net/netip"
	"syscall"
	"time"
)

// ErrBlockedAddress is returned (wrapped in the dial error) when a connection would reach a
// non-public address.
var ErrBlockedAddress = errors.New("address is not public")

// DialControl is a net.Dialer Control function that refuses any non-public address and any
// network other than TCP.
func DialControl(network, address string, _ syscall.RawConn) error {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return ErrBlockedAddress
	}
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || !IsPublicIP(ip) {
		return ErrBlockedAddress
	}
	return nil
}

// NewDialer returns a dialer with the given connect timeout that only reaches public
// addresses.
func NewDialer(timeout time.Duration) *net.Dialer {
	return &net.Dialer{Timeout: timeout, Control: DialControl}
}

// IsPublicIP reports whether ip is a globally routable unicast address: not loopback,
// private (RFC 1918 / ULA), link-local, multicast, unspecified, CGNAT, or one of the IETF
// special-purpose ranges. An IPv6 address that carries an IPv4 one (IPv4-mapped,
// IPv4-compatible, NAT64, 6to4) is judged by the IPv4 address it leads to.
func IsPublicIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	// To4() unwraps IPv4-mapped IPv6 (::ffff:a.b.c.d) too, so these checks cover both.
	if ip4 := ip.To4(); ip4 != nil {
		return isPublicIPv4(ip4)
	}
	addr, ok := netip.AddrFromSlice(ip)
	if !ok || !addr.Is6() {
		return false
	}
	for _, p := range nonPublicIPv6 {
		if p.Contains(addr) {
			return false
		}
	}
	b := addr.As16()
	switch {
	case ipv4Compatible.Contains(addr), nat64.Contains(addr):
		// ::a.b.c.d (deprecated, still routed to IPv4 by some stacks) and the NAT64 well-known
		// prefix: what DNS64 hands an IPv6-only host for an IPv4-only name, so a public
		// address behind it is fine and a private one is not.
		return IsPublicIP(net.IPv4(b[12], b[13], b[14], b[15]))
	case sixToFour.Contains(addr):
		return IsPublicIP(net.IPv4(b[2], b[3], b[4], b[5]))
	}
	return true
}

func isPublicIPv4(ip4 net.IP) bool {
	if ip4.IsLoopback() || ip4.IsPrivate() || ip4.IsUnspecified() || ip4.IsLinkLocalUnicast() ||
		ip4.IsMulticast() || ip4.IsLinkLocalMulticast() {
		return false
	}
	if ip4[0] == 0 { // 0.0.0.0/8 ("this network"; Linux routes it to the local host)
		return false
	}
	if ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127 { // 100.64.0.0/10 (CGNAT)
		return false
	}
	if ip4[0] == 192 && ip4[1] == 0 && ip4[2] == 0 { // 192.0.0.0/24 (IETF)
		return false
	}
	if ip4[0] == 192 && ip4[1] == 88 && ip4[2] == 99 { // 192.88.99.0/24 (6to4 relay anycast)
		return false
	}
	if ip4[0] == 198 && (ip4[1] == 18 || ip4[1] == 19) { // 198.18.0.0/15 (benchmark)
		return false
	}
	if ip4[0] >= 240 { // 240.0.0.0/4 (reserved) and the broadcast address
		return false
	}
	return true
}

var (
	ipv4Compatible = netip.MustParsePrefix("::/96")
	nat64          = netip.MustParsePrefix("64:ff9b::/96")
	sixToFour      = netip.MustParsePrefix("2002::/16")
	// nonPublicIPv6 are the IPv6 special-purpose ranges (RFC 6890 and successors) that are not
	// globally reachable unicast and that the net.IP predicates above do not already cover.
	nonPublicIPv6 = []netip.Prefix{
		netip.MustParsePrefix("64:ff9b:1::/48"), // NAT64 for local use (RFC 8215)
		netip.MustParsePrefix("100::/64"),       // discard-only
		netip.MustParsePrefix("2001::/32"),      // Teredo: the IPv4 end is obfuscated, refuse it whole
		netip.MustParsePrefix("2001:2::/48"),    // benchmarking
		netip.MustParsePrefix("fec0::/10"),      // site-local (deprecated, still routed by some networks)
	}
)
