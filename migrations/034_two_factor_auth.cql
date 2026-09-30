-- Two-factor authentication: TOTP authenticator-app codes, WebAuthn passkeys/security
-- keys, and single-use recovery codes as the fallback when neither is reachable.

ALTER TABLE users ADD totp_enabled boolean;
-- AES-256-GCM(base32 TOTP secret), base64. Unlike the password hash, this must be
-- reversible - the server computes the code to compare - so it is encrypted at rest under
-- TOTP_ENCRYPTION_KEY rather than hashed.
ALTER TABLE users ADD totp_secret text;

-- Single-use recovery codes, issued the first time a user turns on TOTP or adds a passkey.
-- Hashed like a session token (sha256): the server only ever needs to compare one, never
-- show it again.
CREATE TABLE recovery_codes_by_user (
    user_id bigint,
    code_hash text,
    used_at timestamp,
    created_at timestamp,
    PRIMARY KEY (user_id, code_hash)
);

-- WebAuthn credentials (passkeys / security keys), one row per registered authenticator.
-- Partitioned by user: login already knows who is signing in by the time it asks for a
-- passkey (this is a second factor, not discoverable/usernameless login), so no global
-- credential_id -> user lookup table is needed.
--
-- `credential` is the go-webauthn Credential struct, JSON-serialized whole (its public key,
-- attestation, flags and Authenticator{AAGUID, SignCount, CloneWarning} all together) -
-- this is the library's own documented "opaque serialized value" persistence shape. Several
-- of those nested fields (SignCount, CloneWarning, the UserVerified/BackupState flags) MUST
-- be rewritten after every login, which a single blob makes a single UPDATE rather than a
-- column-by-column one.
CREATE TABLE webauthn_credentials_by_user (
    user_id bigint,
    credential_id text,
    credential blob,
    name text,
    created_at timestamp,
    last_used_at timestamp,
    PRIMARY KEY (user_id, credential_id)
);
