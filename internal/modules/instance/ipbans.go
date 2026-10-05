package instance

import (
	"context"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/StrafeChat/equinox/internal/logger"
)

// ipBanRefresh is how stale a process's copy of instance_ip_bans may get before a check
// reloads it. A ban written through this process takes effect at once (the copy is
// dropped); one written by another API replica is enforced here within this long.
const ipBanRefresh = 30 * time.Second

type ipBanEntry struct {
	net *net.IPNet
	ban IPBan
}

// ipBanCache is process-wide, not per Service: route setup builds several instance
// Services (one for /auth, one for /instance, one for Discover), and a ban written through
// the admin routes' Service must be enforced by the one guarding /auth right away.
var ipBanCache struct {
	mu       sync.RWMutex
	entries  []ipBanEntry
	loadedAt time.Time
	loading  bool
}

func invalidateIPBanCache() {
	ipBanCache.mu.Lock()
	ipBanCache.loadedAt = time.Time{}
	ipBanCache.mu.Unlock()
}

// loadIPBans replaces the in-memory copy with the table. Rows that no longer parse (hand
// edited) are skipped, not fatal.
func (s *Service) loadIPBans(ctx context.Context) error {
	rows, err := s.mod.Repo.ListIPBans(ctx)
	if err != nil {
		return err
	}
	entries := make([]ipBanEntry, 0, len(rows))
	for _, b := range rows {
		_, n, perr := net.ParseCIDR(b.CIDR)
		if perr != nil {
			continue
		}
		entries = append(entries, ipBanEntry{net: n, ban: b})
	}
	ipBanCache.mu.Lock()
	ipBanCache.entries = entries
	ipBanCache.loadedAt = time.Now()
	ipBanCache.mu.Unlock()
	return nil
}

// ensureIPBansFresh loads the list on first use (synchronously: the first /auth request
// after a start or a write must see it) and refreshes a stale copy in the background while
// the old one keeps serving.
func (s *Service) ensureIPBansFresh(ctx context.Context) {
	ipBanCache.mu.RLock()
	loaded := ipBanCache.loadedAt
	ipBanCache.mu.RUnlock()
	if loaded.IsZero() {
		if err := s.loadIPBans(ctx); err != nil {
			logger.Err("instance", err, map[string]any{"stage": "load_ip_bans"})
		}
		return
	}
	if time.Since(loaded) < ipBanRefresh {
		return
	}
	ipBanCache.mu.Lock()
	if ipBanCache.loading {
		ipBanCache.mu.Unlock()
		return
	}
	ipBanCache.loading = true
	ipBanCache.mu.Unlock()
	go func() {
		defer func() {
			ipBanCache.mu.Lock()
			ipBanCache.loading = false
			ipBanCache.mu.Unlock()
		}()
		bg, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.loadIPBans(bg); err != nil {
			logger.Err("instance", err, map[string]any{"stage": "refresh_ip_bans"})
		}
	}()
}

// IPBanFor is the ban covering ip, or nil. Fails open: an address that does not parse, or
// a store that cannot be read, lets the request through - the ban list is one layer, the
// account ban and the limiters are the others, and an outage must not lock everyone out.
func (s *Service) IPBanFor(ctx context.Context, ip string) *IPBan {
	if s.mod == nil {
		return nil
	}
	parsed := net.ParseIP(strings.TrimSpace(ip))
	if parsed == nil {
		return nil
	}
	s.ensureIPBansFresh(ctx)
	now := time.Now().UTC()
	ipBanCache.mu.RLock()
	defer ipBanCache.mu.RUnlock()
	for _, e := range ipBanCache.entries {
		if e.ban.Expired(now) {
			continue
		}
		if e.net.Contains(parsed) {
			b := e.ban
			return &b
		}
	}
	return nil
}

// parseBanCIDR accepts an address or a range and returns the canonical network. A bare
// address is the /32 or /128 that holds only it.
func parseBanCIDR(raw string) (*net.IPNet, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, ErrInvalidCIDR
	}
	if !strings.Contains(raw, "/") {
		ip := net.ParseIP(raw)
		if ip == nil {
			return nil, ErrInvalidCIDR
		}
		bits := 8 * net.IPv6len
		if ip4 := ip.To4(); ip4 != nil {
			ip = ip4
			bits = 8 * net.IPv4len
		}
		return &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)}, nil
	}
	_, n, err := net.ParseCIDR(raw)
	if err != nil {
		return nil, ErrInvalidCIDR
	}
	ones, bits := n.Mask.Size()
	min := MinIPv6BanPrefix
	if bits == 8*net.IPv4len {
		min = MinIPv4BanPrefix
	}
	if ones < min {
		return nil, ErrCIDRTooWide
	}
	return n, nil
}

func ipBanAuditReason(cidr, reason string) string {
	if reason == "" {
		return cidr
	}
	return cidr + ": " + reason
}

// BanIP bans an address or range from registering and signing in.
func (s *Service) BanIP(ctx context.Context, actorID int64, in IPBanInput) (*IPBan, error) {
	if err := s.requireAdmin(ctx, actorID); err != nil {
		return nil, err
	}
	n, err := parseBanCIDR(in.CIDR)
	if err != nil {
		return nil, err
	}
	reason := strings.TrimSpace(in.Reason)
	if len([]rune(reason)) > MaxBanReason || in.MaxAgeSeconds < 0 || in.MaxAgeSeconds > MaxBanAgeSeconds {
		return nil, ErrInvalidBan
	}
	var exp *time.Time
	if in.MaxAgeSeconds > 0 {
		e := time.Now().UTC().Add(time.Duration(in.MaxAgeSeconds) * time.Second)
		exp = &e
	}
	return s.banCIDR(ctx, actorID, n, reason, exp)
}

// banCIDR writes one IP ban. A live ban on exactly this range is a conflict; an expired
// one is simply replaced (same key).
func (s *Service) banCIDR(ctx context.Context, actorID int64, n *net.IPNet, reason string, exp *time.Time) (*IPBan, error) {
	cidr := n.String()
	now := time.Now().UTC()
	existing, err := s.mod.Repo.ListIPBans(ctx)
	if err != nil {
		return nil, err
	}
	for _, e := range existing {
		if e.CIDR == cidr && !e.Expired(now) {
			return nil, ErrIPBanExists
		}
	}
	b := &IPBan{CIDR: cidr, BannedBy: actorID, Reason: reason, CreatedAt: now, ExpiresAt: exp}
	if err := s.mod.Repo.CreateIPBan(ctx, b); err != nil {
		return nil, err
	}
	invalidateIPBanCache()
	s.audit(ctx, actorID, AuditIPBan, TargetIP, 0, ipBanAuditReason(cidr, reason))
	return b, nil
}

func (s *Service) UnbanIP(ctx context.Context, actorID int64, raw string) error {
	if err := s.requireAdmin(ctx, actorID); err != nil {
		return err
	}
	n, err := parseBanCIDR(raw)
	if err != nil {
		return err
	}
	cidr := n.String()
	existing, err := s.mod.Repo.ListIPBans(ctx)
	if err != nil {
		return err
	}
	found := false
	for _, e := range existing {
		if e.CIDR == cidr {
			found = true
			break
		}
	}
	if !found {
		return ErrIPBanNotFound
	}
	if err := s.mod.Repo.DeleteIPBan(ctx, cidr); err != nil {
		return err
	}
	invalidateIPBanCache()
	s.audit(ctx, actorID, AuditIPUnban, TargetIP, 0, cidr)
	return nil
}

// ListIPBans is the live list, newest first. Expired rows are cleared on the way past.
func (s *Service) ListIPBans(ctx context.Context, actorID int64) ([]IPBan, error) {
	if err := s.requireAdmin(ctx, actorID); err != nil {
		return nil, err
	}
	all, err := s.mod.Repo.ListIPBans(ctx)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	live := make([]IPBan, 0, len(all))
	for _, b := range all {
		if b.Expired(now) {
			_ = s.mod.Repo.DeleteIPBan(ctx, b.CIDR)
			continue
		}
		live = append(live, b)
	}
	sort.Slice(live, func(i, j int) bool { return live[i].CreatedAt.After(live[j].CreatedAt) })
	return live, nil
}

// publicIP reports whether an address is one worth banning: a session recorded behind a
// misconfigured proxy carries the proxy's private address, and banning that would lock
// out every user at once.
func publicIP(ip net.IP) bool {
	return ip != nil && !ip.IsLoopback() && !ip.IsUnspecified() && !ip.IsPrivate() &&
		!ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() && !ip.IsMulticast()
}

// liveSessionIPs is the distinct public addresses of an account's current sessions - what
// "also ban their IPs" bans. Read before the sessions are revoked.
func (s *Service) liveSessionIPs(ctx context.Context, userID int64) []string {
	sessions, err := s.mod.Sessions.ListByUser(ctx, userID)
	if err != nil {
		logger.Err("instance", err, map[string]any{"stage": "session_ips", "user_id": userID})
		return nil
	}
	now := time.Now().UTC()
	seen := map[string]struct{}{}
	var out []string
	for _, sess := range sessions {
		if !sess.RevokedAt.IsZero() || !sess.ExpiresAt.After(now) {
			continue
		}
		ip := net.ParseIP(strings.TrimSpace(sess.IPAddress))
		if !publicIP(ip) {
			continue
		}
		key := ip.String()
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}
	return out
}

// banIPs bans each address as its own /32 or /128, skipping any a standing ban already
// covers. Returns the ranges written; a failure on one is logged and the rest go on.
func (s *Service) banIPs(ctx context.Context, actorID int64, ips []string, reason string, exp *time.Time) []string {
	var done []string
	for _, raw := range ips {
		if s.IPBanFor(ctx, raw) != nil {
			continue
		}
		n, err := parseBanCIDR(raw)
		if err != nil {
			continue
		}
		b, err := s.banCIDR(ctx, actorID, n, reason, exp)
		if err != nil {
			if err != ErrIPBanExists {
				logger.Err("instance", err, map[string]any{"stage": "ban_session_ip", "cidr": n.String()})
			}
			continue
		}
		done = append(done, b.CIDR)
	}
	return done
}
