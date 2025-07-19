package models

import (
	"time"

	"github.com/scylladb/gocqlx/v2/table"
)

// RoomMemberPermissions represents permission overrides for individual members in specific rooms
// This allows fine-grained control over what specific users can do in individual rooms
type RoomMemberPermissions struct {
	RoomID      string    `db:"room_id" json:"room_id"`
	UserID      string    `db:"user_id" json:"user_id"`
	Permissions int64     `db:"permissions" json:"permissions"` // Bitmap for granted permissions
	Denied      int64     `db:"denied" json:"denied"`           // Bitmap for denied permissions
	CreatedAt   time.Time `db:"created_at" json:"created_at"`
	UpdatedAt   time.Time `db:"updated_at" json:"updated_at"`
}

var roomMemberPermissionsMeta = table.Metadata{
	Name:    "room_member_permissions",
	Columns: []string{"room_id", "user_id", "permissions", "denied", "created_at", "updated_at"},
	PartKey: []string{"room_id"},
	SortKey: []string{"user_id"},
}

var RoomMemberPermissionsTable = table.New(roomMemberPermissionsMeta)

// SchemaDefinition returns the CQL statements to create the table
func (rmp *RoomMemberPermissions) SchemaDefinition() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS room_member_permissions (
			room_id text,
			user_id text,
			permissions bigint,
			denied bigint,
			created_at timestamp,
			updated_at timestamp,
			PRIMARY KEY (room_id, user_id)
		)`,
	}
}

// GetPermissionsBitmap returns the granted permissions bitmap
func (rmp *RoomMemberPermissions) GetPermissionsBitmap() int64 {
	return rmp.Permissions
}

// SetPermissionsBitmap sets the granted permissions bitmap
func (rmp *RoomMemberPermissions) SetPermissionsBitmap(bitmap int64) {
	rmp.Permissions = bitmap
}

// GetDeniedBitmap returns the denied permissions bitmap
func (rmp *RoomMemberPermissions) GetDeniedBitmap() int64 {
	return rmp.Denied
}

// SetDeniedBitmap sets the denied permissions bitmap
func (rmp *RoomMemberPermissions) SetDeniedBitmap(bitmap int64) {
	rmp.Denied = bitmap
}

// HasPermissionsBitmap always returns true since we use bitmaps
func (rmp *RoomMemberPermissions) HasPermissionsBitmap() bool {
	return true
}