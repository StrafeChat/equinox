# Role Management System Migration Guide

This guide explains the migration from the old embedded role system to the new junction table-based role management system in Equinox.

## What Changed

### Before (Old System)
- Roles were stored as a `[]string` field directly in the `space_members` table
- Limited scalability and querying capabilities
- Difficult to track role assignment history

### After (New System)
- Roles are managed through a dedicated junction table system
- Two new tables: `space_member_roles` and `space_member_roles_by_role`
- Better performance for role-based queries
- Support for role assignment metadata (assigned_by, assigned_at)
- Improved scalability

## New Database Schema

### space_member_roles
```sql
CREATE TABLE space_member_roles (
    space_id bigint,
    user_id text,
    role_id text,
    assigned_by text,
    assigned_at timestamp,
    PRIMARY KEY (space_id, user_id, role_id)
);
```

### space_member_roles_by_role
```sql
CREATE TABLE space_member_roles_by_role (
    space_id bigint,
    role_id text,
    user_id text,
    assigned_by text,
    assigned_at timestamp,
    PRIMARY KEY (space_id, role_id, user_id)
);
```

## Migration Process

### Automatic Migration

The system includes automatic migration functions that will:

1. Create the new junction tables
2. Migrate existing role assignments from `space_members.roles` to the new tables
3. Preserve all existing role assignments

### Running the Migration

To run the migration, you can call the migration functions in your application:

```go
import "github.com/StrafeChat/equinox/src/database"

// Run the migration
if err := database.MigrateMemberRolesToJunctionTable(); err != nil {
    log.Fatalf("Migration failed: %v", err)
}

// Verify the migration was successful
if err := database.VerifyMemberRolesMigration(); err != nil {
    log.Printf("Migration verification failed: %v", err)
}
```

### Manual Cleanup

After verifying the migration was successful, you should manually remove the old `roles` column:

```sql
ALTER TABLE space_members DROP roles;
```

**⚠️ Warning**: Only run this command after thoroughly testing that the new role system works correctly!

## New API Usage

### Repository Methods

The new `SpaceMemberRolesRepository` provides these methods:

```go
// Add a role to a member
AddRoleToMember(ctx context.Context, spaceID int64, userID, roleID, assignedBy string) error

// Remove a role from a member
RemoveRoleFromMember(ctx context.Context, spaceID int64, userID, roleID string) error

// Get all roles for a member
GetMemberRoles(ctx context.Context, spaceID int64, userID string) ([]models.SpaceMemberRole, error)

// Get all members with a specific role
GetMembersWithRole(ctx context.Context, spaceID int64, roleID string) ([]models.SpaceMemberRolesByRole, error)

// Set all roles for a member (replaces existing roles)
SetMemberRoles(ctx context.Context, spaceID int64, userID string, roleIDs []string, assignedBy string) error

// Get all role assignments in a space
GetAllSpaceMemberRoles(ctx context.Context, spaceID int64) ([]models.SpaceMemberRole, error)
```

### Handler Updates

The role management handlers have been updated to use the new repository:

- `POST /api/v1/spaces/{spaceId}/members/{userId}/roles/{roleId}` - Add role to member
- `DELETE /api/v1/spaces/{spaceId}/members/{userId}/roles/{roleId}` - Remove role from member
- Role checking in permission validation now uses the new system

## Benefits of the New System

1. **Better Performance**: Dedicated tables optimized for role queries
2. **Scalability**: No longer limited by row size constraints
3. **Audit Trail**: Track who assigned roles and when
4. **Flexible Queries**: Easy to find all members with a role or all roles for a member
5. **Consistency**: Proper relational design following database best practices

## Troubleshooting

### Migration Issues

If the migration fails:

1. Check the logs for specific error messages
2. Ensure the database user has proper permissions
3. Verify the new tables were created successfully
4. Check for any constraint violations

### Verification Failures

If role counts don't match after migration:

1. Check for members with empty or null roles arrays
2. Look for any errors in the migration logs
3. Manually compare a few records between old and new systems

### Rollback

If you need to rollback:

1. The old `roles` column is preserved during migration
2. You can drop the new tables: `DROP TABLE space_member_roles; DROP TABLE space_member_roles_by_role;`
3. Revert the code changes to use the old system

## Testing

After migration, test these scenarios:

1. Adding roles to members
2. Removing roles from members
3. Checking member permissions
4. Listing members with specific roles
5. Getting all roles for a member

Ensure all role-based functionality works as expected before removing the old `roles` column.