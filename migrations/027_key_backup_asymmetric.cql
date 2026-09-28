-- Asymmetric ("write-only") key backup: the Matrix m.megolm_backup.v1.curve25519-aes-sha2
-- scheme, which the crypto engine already implements.
--
-- Why this replaces the symmetric backup in 026: there, the secret that decrypts the whole
-- server-side archive lived on every device (it was the local store passphrase), and the
-- only thing standing between a stolen database and every user's message history was a
-- 6-digit PIN. Here each device holds only the *public* half, so it can keep adding room
-- keys to the backup while being unable to read it - a compromised device (XSS, a stolen
-- laptop) leaks that device's own sessions, not the account's whole history. The private
-- half exists on the server only wrapped under the user's recovery code, which is 128 bits
-- of CSPRNG rather than something a person picks, so offline brute force is not a threat
-- model any more.
--
-- 026's device_key_backups table is deliberately left in place: accounts created before
-- this migration still have their history there, and the client needs to read it once to
-- migrate them over. It deletes that row as soon as the new backup holds the same keys.

-- One row per backup version. `auth_data` is public by design (it carries the Curve25519
-- public key devices encrypt to); `wrapped_private_key` is AES-256-GCM ciphertext under a
-- key derived from the recovery code, so it is opaque to the server.
CREATE TABLE IF NOT EXISTS key_backup_versions (
    user_id bigint,
    version bigint,
    algorithm text,
    auth_data text,
    wrapped_private_key text,
    salt text,
    etag text,
    created_at timestamp,
    PRIMARY KEY (user_id, version)
) WITH CLUSTERING ORDER BY (version DESC);

-- One row per backed-up Megolm session. Partitioned by (user, version) rather than also by
-- room so a restore can page the whole backup in one partition scan - the sizes involved
-- are a few thousand rows of a few hundred bytes for even a heavy account.
--
-- room_id is text, not bigint: these are the crypto engine's identifiers (!<snowflake>:<domain>),
-- and the server never interprets them - it stores and returns them verbatim.
CREATE TABLE IF NOT EXISTS key_backup_sessions (
    user_id bigint,
    version bigint,
    room_id text,
    session_id text,
    session_data text,
    first_message_index int,
    forwarded_count int,
    is_verified boolean,
    created_at timestamp,
    PRIMARY KEY ((user_id, version), room_id, session_id)
);
