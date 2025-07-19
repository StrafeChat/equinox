package models

import (
	"time"

	"github.com/scylladb/gocqlx/v2/table"
)

// RoomRolePermissions represents permission overrides for roles in specific rooms
// This allows fine-grained control over what roles can do in individual rooms
type RoomRolePermissions struct {
	RoomID      string    `db:"room_id" json:"room_id"`
	RoleID      string    `db:"role_id" json:"role_id"`
	Permissions int64     `db:"permissions" json:"permissions"` // Bitmap for granted permissions
	Denied      int64     `db:"denied" json:"denied"`           // Bitmap for denied permissions
	CreatedAt   time.Time `db:"created_at" json:"created_at"`
	UpdatedAt   time.Time `db:"updated_at" json:"updated_at"`
}

var roomRolePermissionsMeta = table.Metadata{
	Name:    "room_role_permissions",
	Columns: []string{"room_id", "role_id", "permissions", "denied", "created_at", "updated_at"},
	PartKey: []string{"room_id"},
	SortKey: []string{"role_id"},
}

var RoomRolePermissionsTable = table.New(roomRolePermissionsMeta)

// SchemaDefinition returns the CQL statements to create the table
func (rrp *RoomRolePermissions) SchemaDefinition() []string {
	return []string{
		`CREATE TABLE IF NOT EXISTS room_role_permissions (
			room_id text,
			role_id text,
			permissions bigint,
			denied bigint,
			created_at timestamp,
			updated_at timestamp,
			PRIMARY KEY (room_id, role_id)
		)`,
	}
}

// GetPermissionsBitmap returns the granted permissions bitmap
func (rrp *RoomRolePermissions) GetPermissionsBitmap() int64 {
	return rrp.Permissions
}

// SetPermissionsBitmap sets the granted permissions bitmap
func (rrp *RoomRolePermissions) SetPermissionsBitmap(bitmap int64) {
	rrp.Permissions = bitmap
}

// GetDeniedBitmap returns the denied permissions bitmap
func (rrp *RoomRolePermissions) GetDeniedBitmap() int64 {
	return rrp.Denied
}

// SetDeniedBitmap sets the denied permissions bitmap
func (rrp *RoomRolePermissions) SetDeniedBitmap(bitmap int64) {
	rrp.Denied = bitmap
}

// HasPermissionsBitmap always returns true since we use bitmaps
func (rrp *RoomRolePermissions) HasPermissionsBitmap() bool {
	return true
}