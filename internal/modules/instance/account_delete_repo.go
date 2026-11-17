package instance

import (
	"context"

	"github.com/scylladb/gocqlx/v3"
)

// accountPartitions drops the per-user partitions a deleted account leaves that no module's
// teardown removes on its own: room read state and notify settings (rooms_by_user - the
// 1:1 PMs' room_participants rows are kept on purpose), mention counters, the space and
// thread membership indexes, and the nicknames the account gave its friends.
type accountPartitions struct {
	session gocqlx.Session
}

func NewAccountPartitions(session gocqlx.Session) AccountPartitions {
	return &accountPartitions{session: session}
}

var accountPartitionDeletes = []string{
	"DELETE FROM rooms_by_user WHERE user_id = ?",
	"DELETE FROM room_mention_counts WHERE user_id = ?",
	"DELETE FROM spaces_by_user WHERE user_id = ?",
	"DELETE FROM thread_members_by_user WHERE user_id = ?",
	"DELETE FROM relationship_nicknames WHERE user_id = ?",
}

func (r *accountPartitions) DeleteUserPartitions(ctx context.Context, userID int64) error {
	var firstErr error
	for _, stmt := range accountPartitionDeletes {
		if err := r.session.Session.Query(stmt, userID).WithContext(ctx).Exec(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
