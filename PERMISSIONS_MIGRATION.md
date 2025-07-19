# Permissions Migration Guide

This guide explains how to migrate permissions data from the old array-based format to the new bitmap format.

## Overview

The migration converts permissions from this format:
```json
["VIEW_CHANNELS", "SEND_MESSAGES", "READ_MESSAGE_HISTORY", "ADD_REACTIONS", "ATTACH_FILES", "EMBED_LINKS", "CONNECT", "SPEAK", "USE_VOICE_ACTIVATION"]
```

To a bitmap format (int64) where each permission is represented by a bit flag.

## Important Changes

- **VIEW_CHANNELS** permission is automatically converted to **VIEW_ROOMS**
- All permissions are converted from string arrays to bitmap integers
- Backup tables are created for safety

## Running the Migration

### Option 1: Standalone Migration Command

```bash
cd equinox
go run src/cmd/migrate_permissions.go
```

### Option 2: As Part of Full Migration

```bash
cd equinox
go run -c "database.MigrateAllSchemas()"
```

### Option 3: Programmatically

```go
import "github.com/StrafeChat/equinox/src/database"

// Initialize database first
if err := database.InitDB(); err != nil {
    log.Fatal(err)
}

// Run permissions migration
if err := database.MigratePermissionsToBitmap(); err != nil {
    log.Fatal(err)
}

// Verify migration
if err := database.VerifyPermissionsMigration(); err != nil {
    log.Fatal(err)
}
```

## What the Migration Does

1. **Backup Creation**: Creates backup tables (`space_roles_backup`) before making changes
2. **Schema Update**: Drops and recreates tables with bitmap permission columns
3. **Data Conversion**: 
   - Converts `VIEW_CHANNELS` to `VIEW_ROOMS`
   - Converts permission arrays to bitmap integers
   - Preserves all other role data (name, color, position, etc.)
4. **Verification**: Checks that migration completed successfully

## Tables Affected

- `space_roles` - Space role permissions
- `room_member_permissions` - Individual user permissions in rooms (schema verification)
- `room_role_permissions` - Role permissions in rooms (schema verification)

## Permission Mapping

The migration uses the following bitmap values:

| Permission | Bitmap Value |
|------------|-------------|
| ADMINISTRATOR | 1 << 0 |
| VIEW_ROOMS (was VIEW_CHANNELS) | 1 << 1 |
| MANAGE_CHANNELS | 1 << 2 |
| MANAGE_ROLES | 1 << 3 |
| ... | ... |

## Safety Features

- **Backup Tables**: Original data is preserved in `*_backup` tables
- **Verification**: Migration includes verification step to ensure data integrity
- **Rollback**: If needed, you can restore from backup tables

## Rollback Procedure (if needed)

```sql
-- Only if migration fails and you need to rollback
DROP TABLE IF EXISTS space_roles;
ALTER TABLE space_roles_backup RENAME TO space_roles;
```

## Post-Migration Cleanup

After verifying the migration was successful, you can remove backup tables:

```sql
DROP TABLE IF EXISTS space_roles_backup;
```

## Troubleshooting

### Migration Fails with "Table doesn't exist"
This likely means your tables already use the bitmap format. The migration will skip tables that are already migrated.

### Permission counts don't match
Check the migration logs for any errors during conversion. The verification step will report mismatches.

### VIEW_CHANNELS still appears
The migration specifically converts `VIEW_CHANNELS` to `VIEW_ROOMS`. If you still see `VIEW_CHANNELS`, check that the migration completed successfully.

## Verification

The migration includes automatic verification that:
- All roles were migrated
- Permission counts match between old and new format
- No `VIEW_CHANNELS` permissions remain
- All permissions are properly converted to bitmap format

## Example Output

```
Starting comprehensive permissions migration to bitmap format...
Checking space_roles table for array-based permissions...
Found 15 space roles with array permissions to migrate
Created backup table: space_roles_backup
Migrated role @everyone in space 123: [VIEW_CHANNELS SEND_MESSAGES] -> 6 (converted VIEW_CHANNELS to VIEW_ROOMS)
Successfully migrated 15 space roles
Verifying permissions migration...
Found 15 space roles with bitmap permissions
✅ Permissions migration verification completed
```