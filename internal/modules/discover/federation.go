package discover

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/StrafeChat/equinox/internal/id"
	"github.com/StrafeChat/equinox/internal/logger"
	"github.com/StrafeChat/equinox/internal/modules/spaces"
	"github.com/StrafeChat/equinox/internal/safego"
)

// Discover across instances. The directory is pulled, not pushed: every instance serves
// what it lists at a signed endpoint, and each one asks the instances it federates with
// for theirs on the maintenance timer, keeping the answer until the next round.
//
// Pull suits a small, slow-changing, public-by-approval list: nothing has to be relayed,
// retried or deleted in a fan-out, a peer that is down only means a card is a little
// stale, and the instance that hosts a space stays the only authority on whether it is
// listed at all. Sharing is per listing (the managers decide) under the operator's
// DISCOVER_FEDERATION switch.

// Federator is the federation engine, as this module needs it. Implemented by
// federation.Service; nil on an instance that does not federate, which turns every
// cross-instance path here into "there is nothing out there".
type Federator interface {
	// PeerDomains is every instance this one may talk to right now.
	PeerDomains(ctx context.Context) []string
	// FetchDirectory asks one peer for the spaces it lists.
	FetchDirectory(ctx context.Context, domain string) ([]RemoteListing, error)
	// JoinListedSpace adds a local user to a space that `domain` lists publicly, building
	// (or extending) this instance's mirror of it from the answer.
	JoinListedSpace(ctx context.Context, domain string, originSpaceID, userID int64) (*spaces.Space, error)
}

func (s *Service) SetFederator(f Federator) { s.fed = f }

// FederationEnabled reports whether this instance shares and shows directories at all.
func (s *Service) FederationEnabled() bool {
	return s.cfg != nil && s.cfg.Flags.DiscoverFederation && s.fed != nil
}

// SharedListings is what this instance tells a peer it lists: every approved space whose
// managers left it shared. Bots are never shared - installing one is an OAuth flow on the
// instance the application lives on, which is not something another instance can offer.
func (s *Service) SharedListings(ctx context.Context) ([]RemoteListing, error) {
	if !s.FederationEnabled() {
		return []RemoteListing{}, nil
	}
	entries, err := s.approved(ctx, KindSpace)
	if err != nil {
		return nil, err
	}
	out := make([]RemoteListing, 0, len(entries))
	for i := range entries {
		e := entries[i]
		if !e.Listing.Federated() || e.Space == nil {
			continue
		}
		out = append(out, RemoteListing{
			SpaceID:     e.Listing.ID,
			Name:        e.Space.Name,
			NameAcronym: e.Space.NameAcronym,
			Icon:        e.Space.Icon,
			Banner:      e.Space.Banner,
			Description: e.Space.Description,
			Tagline:     e.Tagline,
			Tags:        e.Tags,
			MemberCount: e.Space.MemberCount,
			OnlineCount: e.Space.OnlineCount,
		})
		if len(out) >= MaxRemotePerPeer {
			break
		}
	}
	return out, nil
}

// SharesSpace reports whether a space hosted here is listed AND shared - what the origin
// checks before letting a peer's user join it from their Discover page.
func (s *Service) SharesSpace(ctx context.Context, spaceID int64) (bool, error) {
	if !s.FederationEnabled() {
		return false, nil
	}
	l, err := s.repo.Get(ctx, KindSpace, spaceID)
	if err != nil || l == nil {
		return false, err
	}
	return l.Status == StatusApproved && l.Federated(), nil
}

// RefreshRemote asks every peer for its directory and replaces what we held for each.
// Called on the maintenance timer; one unreachable peer never stops the others.
func (s *Service) RefreshRemote(ctx context.Context) {
	if !s.FederationEnabled() {
		return
	}
	for _, domain := range s.fed.PeerDomains(ctx) {
		pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		listings, err := s.fed.FetchDirectory(pctx, domain)
		cancel()
		if err != nil {
			// Keep what we had: a peer that is briefly down should not empty its shelf.
			logger.Warn("discover", "directory from %s: %v (keeping the last copy)", domain, err)
			continue
		}
		if len(listings) > MaxRemotePerPeer {
			listings = listings[:MaxRemotePerPeer]
		}
		now := time.Now().UTC()
		for i := range listings {
			listings[i].Domain = domain
			listings[i].FetchedAt = now
			listings[i].Tagline = clampLine(listings[i].Tagline, MaxTagline)
			listings[i].Name = clampLine(listings[i].Name, 100)
			listings[i].Description = clampLine(listings[i].Description, 1000)
			listings[i].Tags = clampTags(listings[i].Tags)
		}
		if err := s.repo.ReplaceRemote(ctx, domain, listings); err != nil {
			logger.Err("discover", err, map[string]any{"stage": "store_remote", "peer": domain})
		}
	}
	s.invalidate(ctx)
}

// StartDirectoryRefresh keeps the peers' directories current: once shortly after start,
// so a fresh instance has something to show, then on a slow timer. The page is read from
// whatever the last round stored, so a peer being slow or down is never felt here.
func (s *Service) StartDirectoryRefresh(ctx context.Context) {
	if !s.FederationEnabled() {
		return
	}
	safego.Go("discover", func() {
		timer := time.NewTimer(directoryRefreshDelay)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			rctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			s.RefreshRemote(rctx)
			cancel()
			timer.Reset(directoryRefreshInterval)
		}
	})
}

const (
	// Long enough for the instance to finish starting, short enough that a new deployment
	// has its neighbours' spaces on the page within a minute.
	directoryRefreshDelay = 45 * time.Second
	// A directory changes when an administrator approves something: minutes-stale is fine.
	directoryRefreshInterval = 15 * time.Minute
)

// ForgetPeer drops a peer's listings (blocked, or no longer federated).
func (s *Service) ForgetPeer(ctx context.Context, domain string) error {
	if err := s.repo.DropRemote(ctx, domain); err != nil {
		return err
	}
	s.invalidate(ctx)
	return nil
}

// remoteEntries is every peer listing worth showing, as directory cards. Listings from a
// peer that is no longer allowed are skipped (and cleaned up on the next refresh).
func (s *Service) remoteEntries(ctx context.Context) []Entry {
	if !s.FederationEnabled() {
		return nil
	}
	rows, err := s.repo.ListRemote(ctx)
	if err != nil {
		logger.Err("discover", err, map[string]any{"stage": "list_remote"})
		return nil
	}
	allowed := map[string]bool{}
	for _, d := range s.fed.PeerDomains(ctx) {
		allowed[d] = true
	}
	out := make([]Entry, 0, len(rows))
	for i := range rows {
		r := rows[i]
		if !allowed[r.Domain] {
			continue
		}
		out = append(out, Entry{
			Listing: Listing{
				Kind:   KindSpace,
				ID:     r.SpaceID,
				Status: StatusApproved,
				// A card only reaches us because its origin shares it, so the flag the
				// managers set over there is true by construction.
				Federate: true,
				Tagline:  r.Tagline,
				Tags:     append([]string{}, r.Tags...),
			},
			Space: &SpaceCard{
				OriginDomain: r.Domain,
				ID:           id.Format(r.SpaceID),
				Name:         r.Name,
				NameAcronym:  r.NameAcronym,
				Icon:         r.Icon,
				Banner:       r.Banner,
				Description:  r.Description,
				MemberCount:  r.MemberCount,
				OnlineCount:  r.OnlineCount,
			},
		})
	}
	return out
}

// JoinRemoteSpace joins a space another instance lists. The listing must be one we were
// told about, so this can never be used to join an arbitrary space elsewhere; the origin
// checks again that it really is listed and shared before adding the member.
func (s *Service) JoinRemoteSpace(ctx context.Context, actorID int64, domain string, originSpaceID int64) (*spaces.Space, error) {
	if !s.FederationEnabled() {
		return nil, ErrNotApproved
	}
	known, err := s.repo.GetRemote(ctx, domain, originSpaceID)
	if err != nil {
		return nil, err
	}
	if known == nil {
		return nil, ErrNotApproved
	}
	sp, err := s.fed.JoinListedSpace(ctx, domain, originSpaceID, actorID)
	if err != nil {
		return nil, err
	}
	s.invalidate(ctx)
	return sp, nil
}

func clampLine(s string, max int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if r := []rune(s); len(r) > max {
		return string(r[:max])
	}
	return s
}

func clampTags(tags []string) []string {
	out := make([]string, 0, MaxTags)
	for _, t := range tags {
		t = clampLine(t, MaxTagLen)
		if t == "" {
			continue
		}
		out = append(out, t)
		if len(out) >= MaxTags {
			break
		}
	}
	sort.Strings(out)
	return out
}
