package devices

import (
	"context"
	"encoding/json"
	"strings"
)

// KeyRouter lets the device-key endpoints reach users on other instances. The crypto
// engine addresses everyone as @<id>:<server>; when <server> is another instance the
// request for that user is forwarded there (signed) instead of looked up locally.
// nil when federation is off - then every server part is treated as local.
type KeyRouter interface {
	IsLocalServer(server string) bool
	QueryRemote(ctx context.Context, domain string, req map[string][]string) (*QueryKeysOutput, error)
	ClaimRemote(ctx context.Context, domain string, req map[string]map[string]string) (*ClaimKeysOutput, error)
	SendToDeviceRemote(ctx context.Context, domain string, senderUserID, senderDeviceID int64, eventType string, messages map[string]map[string]json.RawMessage) error
	// LocalFID is the federated id of a local user, or "" when federation is off.
	LocalFID(userID int64) string
}

// FIDResolver maps a federated id to the local user id it corresponds to (the user's own
// id, or their shadow row's id for remote users).
type FIDResolver interface {
	ResolveLocalID(ctx context.Context, fid string) (int64, error)
}

// SetRouter wires federation into the service.
func (s *Service) SetRouter(r KeyRouter) {
	s.router = r
}

// splitMatrixUserID separates "@id:server" into its parts. Bare ids have an empty server.
func splitMatrixUserID(s string) (idPart, server string) {
	if len(s) > 1 && s[0] == '@' {
		if i := strings.IndexByte(s, ':'); i > 1 {
			return s[1:i], strings.ToLower(s[i+1:])
		}
		return s[1:], ""
	}
	return s, ""
}

// isRemoteServer reports whether a server part belongs to another instance.
func (s *Service) isRemoteServer(server string) bool {
	if server == "" || s.router == nil {
		return false
	}
	return !s.router.IsLocalServer(server)
}

func (s *Service) localFID(userID int64) string {
	if s.router == nil {
		return ""
	}
	return s.router.LocalFID(userID)
}
