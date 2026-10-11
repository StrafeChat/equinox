package pushgateway

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// proxyList decides which peers' X-Forwarded-For is believed. Same vocabulary as the
// API's TRUSTED_PROXIES: IPs, CIDRs, and the words loopback / linklocal / private.
type proxyList struct {
	prefixes  []netip.Prefix
	loopback  bool
	linkLocal bool
	private   bool
}

func newProxyList(entries []string) (*proxyList, error) {
	pl := &proxyList{}
	for _, e := range entries {
		switch strings.ToLower(e) {
		case "loopback":
			pl.loopback = true
		case "linklocal":
			pl.linkLocal = true
		case "private":
			pl.private = true
		default:
			if p, err := netip.ParsePrefix(e); err == nil {
				pl.prefixes = append(pl.prefixes, p.Masked())
				continue
			}
			a, err := netip.ParseAddr(e)
			if err != nil {
				return nil, fmt.Errorf("%q is not an IP, CIDR, loopback, linklocal or private", e)
			}
			a = a.Unmap()
			pl.prefixes = append(pl.prefixes, netip.PrefixFrom(a, a.BitLen()))
		}
	}
	return pl, nil
}

func (pl *proxyList) empty() bool {
	return len(pl.prefixes) == 0 && !pl.loopback && !pl.linkLocal && !pl.private
}

func (pl *proxyList) trusted(a netip.Addr) bool {
	a = a.Unmap()
	if (pl.loopback && a.IsLoopback()) ||
		(pl.linkLocal && (a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast())) ||
		(pl.private && a.IsPrivate()) {
		return true
	}
	for _, p := range pl.prefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// clientIP is the address per-IP limits apply to. Without trusted proxies it is the TCP
// peer. Behind them, X-Forwarded-For is walked from the right - the end our own proxies
// appended to - and the first address that is not one of them is the client; anything to
// its left was supplied by the client and could say anything.
func (pl *proxyList) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	peer = peer.Unmap()
	if pl.empty() || !pl.trusted(peer) {
		return peer.String()
	}
	var hops []string
	for _, h := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(h, ",")...)
	}
	client := peer
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			break
		}
		client = a.Unmap()
		if !pl.trusted(client) {
			break
		}
	}
	return client.String()
}
