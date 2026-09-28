-- Blocked users: a one-directional set on the blocker's own row, the same shape as the
-- `relationships` (friends) and `spaces` sets. A block hides the blocked user's messages,
-- stops them sending friend requests, and lists them under Blocked.
ALTER TABLE users ADD blocks set<bigint>;
