// Package federation lets independent StrafeChat instances talk to each other.
//
// Model in one paragraph: every instance has a domain and an Ed25519 signing key it
// publishes at https://<domain>/.well-known/strafe. Users are addressed across instances
// as `name#0001@domain`, and carry a federated id (FID) `@<id>:<domain>` where <id> is
// what their *home* instance calls them. A remote user gets a local "shadow" users row so
// the rest of the codebase keeps working with plain local ids; the FID ↔ shadow mapping
// lives in users_by_remote. Rooms are mirrored: each participating instance stores its
// own copy of the room and messages under local ids, tied together by the room's origin
// (origin_domain, origin_room_id) and per-message origin ids. Every server-to-server
// request is signed (see signing.go) and verified against the peer's published key.
package federation

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/StrafeChat/equinox/internal/id"
)

// LegacyServer is the synthetic server name the E2EE engine used before federation
// existed (@<id>:strafe.internal). Treated as "this instance" everywhere so an instance
// that never enables federation keeps working with clients that still send it.
const LegacyServer = "strafe.internal"

var (
	ErrInvalidHandle      = errors.New("invalid handle - expected name#0001@domain")
	ErrInvalidFID         = errors.New("invalid federated id - expected @id:domain")
	ErrPeerNotAllowed     = errors.New("that instance is not allowed by this server's federation policy")
	ErrFederationOff      = errors.New("federation is not enabled on this instance")
	ErrRemoteUserNotFound = errors.New("no such user on that instance")
	ErrRemoteUnavailable  = errors.New("the other instance could not be reached")
)

var handleRe = regexp.MustCompile(`^([A-Za-z0-9_.\-]{2,32})#(\d{1,4})(?:@([A-Za-z0-9.\-]+(?::\d+)?))?$`)

// Handle is a user address as people type it: name#0001@domain (domain optional = local).
type Handle struct {
	Username      string
	Discriminator int
	Domain        string
}

func ParseHandle(s string) (Handle, error) {
	s = strings.TrimSpace(strings.TrimPrefix(s, "@"))
	m := handleRe.FindStringSubmatch(s)
	if m == nil {
		return Handle{}, ErrInvalidHandle
	}
	d, err := strconv.Atoi(m[2])
	if err != nil || d < 1 || d > 9999 {
		return Handle{}, ErrInvalidHandle
	}
	return Handle{Username: m[1], Discriminator: d, Domain: strings.ToLower(m[3])}, nil
}

func (h Handle) String() string {
	s := fmt.Sprintf("%s#%04d", h.Username, h.Discriminator)
	if h.Domain != "" {
		s += "@" + h.Domain
	}
	return s
}

// FormatFID builds the federated id the E2EE engine and peers use for a user.
func FormatFID(originID int64, domain string) string {
	return "@" + id.Format(originID) + ":" + domain
}

// ParseFID splits "@id:domain" into its parts. The domain may be empty for bare ids.
func ParseFID(s string) (originID int64, domain string, err error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "@") {
		// A bare snowflake is accepted as "local" for callers that never learned FIDs.
		v, perr := id.Parse(s)
		if perr != nil {
			return 0, "", ErrInvalidFID
		}
		return v, "", nil
	}
	body := s[1:]
	i := strings.IndexByte(body, ':')
	if i < 0 {
		v, perr := id.Parse(body)
		if perr != nil {
			return 0, "", ErrInvalidFID
		}
		return v, "", nil
	}
	v, perr := id.Parse(body[:i])
	if perr != nil {
		return 0, "", ErrInvalidFID
	}
	return v, strings.ToLower(body[i+1:]), nil
}

// RoomFID is the global room identity the E2EE engine keys Megolm sessions by:
// !<origin_room_id>:<origin_domain>. Every participating instance must agree on it.
func RoomFID(originRoomID int64, originDomain string) string {
	return "!" + id.Format(originRoomID) + ":" + originDomain
}
