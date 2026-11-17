package instance

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/StrafeChat/equinox/internal/modules/applications"
	"github.com/StrafeChat/equinox/internal/modules/auth"
	"github.com/StrafeChat/equinox/internal/modules/relationships"
	"github.com/StrafeChat/equinox/internal/modules/rooms"
	"github.com/StrafeChat/equinox/internal/modules/spaces"
)

// Account deletion by an instance administrator: types, errors and the narrow interfaces
// the pipeline (account_delete.go) reaches the other modules through. Each module owns its
// own teardown; these interfaces exist so tests can fake them and so nothing here creates
// an import cycle (none of those modules imports instance).

const (
	AuditUserDelete        = "user_delete"
	AuditBotDelete         = "bot_delete"
	AuditApplicationDelete = "application_delete"

	// ResolutionAccountDeleted closes an open report about an account that was deleted.
	ResolutionAccountDeleted = "account_deleted"

	DeletionKindUser = "user"
	DeletionKindBot  = "bot"

	// Refusals: why an account cannot be deleted at all (no override).
	RefusalSelf   = "self"
	RefusalSystem = "system"
	RefusalAdmin  = "admin"
	RefusalRemote = "remote"
)

var (
	ErrConfirmMismatch     = errors.New("type the account's username exactly to confirm")
	ErrDeleteSelf          = errors.New("you cannot delete your own account here")
	ErrDeleteSystem        = errors.New("the instance's official account cannot be deleted")
	ErrDeleteAdmin         = errors.New("that account is an instance administrator and cannot be deleted; there is no endpoint to demote one - remove it from INSTANCE_ADMINS, or clear its instance_admin flag in the database, first")
	ErrDeletionInProgress  = errors.New("this account is already being deleted")
	ErrDeletionUnavailable = errors.New("account deletion is not available on this instance")
)

// AccountSpaces is the spaces module's part (spaces.Service).
type AccountSpaces interface {
	AccountSpaceIDs(ctx context.Context, userID int64, known []int64) ([]int64, error)
	OwnedSpaces(ctx context.Context, userID int64, spaceIDs []int64) ([]spaces.OwnedSpace, error)
	RemoveAccount(ctx context.Context, userID, spaceID int64) (string, error)
}

// AccountRooms is what the pipeline reads and writes of rooms directly (rooms.Repository).
type AccountRooms interface {
	ListByUser(ctx context.Context, userID int64) ([]rooms.RoomRow, error)
	GetByIDs(ctx context.Context, ids []int64) ([]*rooms.Room, error)
	ListUserThreadIDs(ctx context.Context, userID int64) ([]int64, error)
	RemoveThreadMember(ctx context.Context, roomID, userID int64) error
}

// GroupLeaver is a group PM's self-leave (rooms.Service.RemoveParticipant): the member_left
// line, ROOM_UPDATE/ROOM_LEAVE, the voice hook, SESSION_ROTATE and the federation relay.
type GroupLeaver interface {
	RemoveParticipant(ctx context.Context, actorID, roomID, targetID int64) error
}

// AccountRelationships is the relationships module's part (relationships.Service).
type AccountRelationships interface {
	RequestCounterparts(ctx context.Context, userID int64) (incoming, outgoing []int64, err error)
	PurgeUser(ctx context.Context, u *auth.User) (relationships.PurgeCounts, error)
}

// AccountDevices is the E2EE material (devices.Service).
type AccountDevices interface {
	CountDevices(ctx context.Context, userID int64) (int, error)
	PurgeUser(ctx context.Context, userID int64) (int, error)
}

// AccountOAuth is the grants the account gave (oauth.Service).
type AccountOAuth interface {
	CountGrants(ctx context.Context, userID int64) (int, error)
	PurgeUser(ctx context.Context, userID int64) (int, error)
}

// AccountPush is the account's push subscriptions (push.Purger).
type AccountPush interface {
	CountSubscriptions(ctx context.Context, userID int64) (int, error)
	PurgeUser(ctx context.Context, userID int64) (int, error)
}

// AccountApps is the developer platform's part (applications.Service).
type AccountApps interface {
	ListByOwner(ctx context.Context, ownerID int64) ([]applications.Application, error)
	AppForBot(ctx context.Context, botUserID int64) (*applications.Application, error)
	RevokeBotToken(ctx context.Context, appID int64) error
	AdminDelete(ctx context.Context, appID int64) error
}

// AccountVoice ends a call the account is in (voice.Service.Leave). Nil when voice is off.
type AccountVoice interface {
	Leave(ctx context.Context, userID int64) error
}

// AccountFederation is the federation engine's part. Nil when federation is off.
type AccountFederation interface {
	DeletionAudience(ctx context.Context, u *auth.User, requestCounterparts []int64) map[string]struct{}
	AfterAccountDeleted(ctx context.Context, u *auth.User, domains map[string]struct{})
}

// AccountPartitions drops the per-user partitions nothing else deletes (account_delete_repo.go).
type AccountPartitions interface {
	DeleteUserPartitions(ctx context.Context, userID int64) error
}

// AccountDeletionDeps is everything the deletion pipeline needs beyond ModerationDeps. Set
// with SetAccountDeletion; without it the endpoints answer ErrDeletionUnavailable. Voice,
// Federation, Partitions and the func fields may be nil.
type AccountDeletionDeps struct {
	Spaces        AccountSpaces
	Rooms         AccountRooms
	Groups        GroupLeaver
	Relationships AccountRelationships
	Devices       AccountDevices
	OAuth         AccountOAuth
	Push          AccountPush
	Apps          AccountApps
	Voice         AccountVoice
	Federation    AccountFederation
	Partitions    AccountPartitions
	// Broadcast tells the account's friends and rooms it was deleted (users.BroadcastDeleted).
	Broadcast func(ctx context.Context, u *auth.User)
	// Publish sends an event to one account's own channel (stargate.PublishToUser); used for
	// SESSION_REVOKED. Defaults to the moderation Redis when nil.
	Publish func(ctx context.Context, userID int64, event string, data map[string]interface{})
	// DeleteProfileImages removes the account's current avatar and banner from this
	// instance's object store, best-effort.
	DeleteProfileImages func(ctx context.Context, u *auth.User)
}

// SetAccountDeletion wires the deletion pipeline's dependencies.
func (s *Service) SetAccountDeletion(d AccountDeletionDeps) { s.del = &d }

// DeleteAccountInput is the body of DELETE /instance/users/:id.
type DeleteAccountInput struct {
	ConfirmUsername    string `json:"confirm_username"`
	TakeDownSpaces     bool   `json:"take_down_spaces"`
	DeleteApplications bool   `json:"delete_applications"`
}

// DeletionCounts is what the preview expects to remove and the result says it removed.
// Messages are not counted: they have no index by sender, and they stay.
type DeletionCounts struct {
	Sessions          int `json:"sessions"`
	Devices           int `json:"devices"`
	Spaces            int `json:"spaces"`
	Friends           int `json:"friends"`
	PendingRequests   int `json:"pending_requests"`
	GroupPMs          int `json:"group_pms"`
	PMs               int `json:"pms"`
	AuthorizedApps    int `json:"authorized_apps"`
	PushSubscriptions int `json:"push_subscriptions"`
	OpenReportsAbout  int `json:"open_reports_about"`
	RemoteInstances   int `json:"remote_instances"`
}

// auditLine is the counts part of the audit entry's reason: numbers only, never content.
func (c DeletionCounts) auditLine() string {
	parts := []string{
		fmt.Sprintf("spaces %d", c.Spaces),
		fmt.Sprintf("friends %d", c.Friends),
		fmt.Sprintf("requests %d", c.PendingRequests),
		fmt.Sprintf("group PMs %d", c.GroupPMs),
		fmt.Sprintf("PMs %d", c.PMs),
		fmt.Sprintf("devices %d", c.Devices),
		fmt.Sprintf("sessions %d", c.Sessions),
		fmt.Sprintf("authorized apps %d", c.AuthorizedApps),
		fmt.Sprintf("push %d", c.PushSubscriptions),
		fmt.Sprintf("reports closed %d", c.OpenReportsAbout),
		fmt.Sprintf("instances told %d", c.RemoteInstances),
	}
	return strings.Join(parts, ", ")
}

// OwnedSpaceBlocker is a space the account owns. HostedOn is "" for a space hosted here
// (resolvable with take_down_spaces) and the origin's domain for one hosted elsewhere
// (unresolvable here: its owner must transfer it, or that instance take it down).
type OwnedSpaceBlocker struct {
	ID          int64
	Name        string
	MemberCount int
	HostedOn    string
}

// OwnedApplicationBlocker is an application the account owns (resolvable with
// delete_applications, which deletes its bot account too).
type OwnedApplicationBlocker struct {
	ID            int64
	Name          string
	BotUserID     int64
	BotUsername   string
	BotSpaceCount int
}

// DeletionBlockers is what stands in the way of a deletion.
type DeletionBlockers struct {
	OwnedSpaces       []OwnedSpaceBlocker
	OwnedApplications []OwnedApplicationBlocker
}

func (b DeletionBlockers) hasRemoteSpace() bool {
	for _, sp := range b.OwnedSpaces {
		if sp.HostedOn != "" {
			return true
		}
	}
	return false
}

func (b DeletionBlockers) hasLocalSpace() bool {
	for _, sp := range b.OwnedSpaces {
		if sp.HostedOn == "" {
			return true
		}
	}
	return false
}

// blocks reports whether the blockers stop a deletion with these overrides.
func (b DeletionBlockers) blocks(in DeleteAccountInput) bool {
	return b.hasRemoteSpace() ||
		(b.hasLocalSpace() && !in.TakeDownSpaces) ||
		(len(b.OwnedApplications) > 0 && !in.DeleteApplications)
}

// DeletionBlockedError refuses a deletion the blockers stop (409, with the blockers).
type DeletionBlockedError struct {
	Blockers DeletionBlockers
}

func (e *DeletionBlockedError) Error() string { return "this account owns spaces or applications" }

// DeletionStageError is a step that must succeed and did not (500, with the stage). Every
// step is idempotent, so the same request finishes the job.
type DeletionStageError struct {
	Stage string
	Err   error
}

func (e *DeletionStageError) Error() string { return e.Stage + ": " + e.Err.Error() }
func (e *DeletionStageError) Unwrap() error { return e.Err }

// BotApplication is, for a bot, the application it belongs to. Orphan: the application was
// already deleted (ID 0, Name "").
type BotApplication struct {
	ID     int64
	Name   string
	Owner  *auth.User
	Orphan bool
}

// DeletionPreview is GET /instance/users/:id/deletion-preview.
type DeletionPreview struct {
	User          *auth.User
	InstanceAdmin bool
	Kind          string
	Refusal       string
	Blockers      DeletionBlockers
	Application   *BotApplication
	Counts        DeletionCounts
}

// DeletedSpace / DeletedApplication name what a deletion took with it.
type DeletedSpace struct {
	ID   int64
	Name string
}

type DeletedApplication struct {
	ID        int64
	Name      string
	BotUserID int64
}

// DeletionResult is DELETE /instance/users/:id's answer.
type DeletionResult struct {
	UserID              int64
	Username            string
	Kind                string
	Counts              DeletionCounts
	SpacesTakenDown     []DeletedSpace
	ApplicationsDeleted []DeletedApplication
	Warnings            []string
	// BanRemoved: the account had an instance ban, which went with it (an IP ban did not).
	BanRemoved bool
}
