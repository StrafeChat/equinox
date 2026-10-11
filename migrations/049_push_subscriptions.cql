-- Push notifications (internal/modules/push): one Web Push subscription per signed-in
-- session. The endpoint is wherever the device asked to be reached (a push gateway for an
-- app build, a UnifiedPush distributor, a browser's push service) and p256dh/auth are the
-- device's RFC 8291 keys - every push body is encrypted to them, so nothing in between
-- reads it. Re-subscribing replaces the row; revoking the session deletes it.
--
-- The prefs_* columns are the device's own notification choices (preview text, PM and
-- space modes, calls), kept here because the decision to notify is made on the server
-- while the app is closed. failures counts consecutive rejected deliveries; a
-- subscription the push service keeps refusing is dropped.
CREATE TABLE IF NOT EXISTS push_subscriptions (
    user_id bigint,
    session_id bigint,
    endpoint text,
    p256dh text,
    auth text,
    kind text,
    pref_preview boolean,
    pref_pm text,
    pref_space text,
    pref_calls boolean,
    created_at timestamp,
    updated_at timestamp,
    last_success_at timestamp,
    last_error text,
    failures int,
    PRIMARY KEY ((user_id), session_id)
);

-- The instance's generated VAPID (RFC 8292) key when PUSH_VAPID_PRIVATE_KEY is not set:
-- claimed once with a lightweight transaction so every API replica signs with the same
-- key. private_key is base64url of the raw 32-byte P-256 scalar.
CREATE TABLE IF NOT EXISTS push_vapid (
    key text PRIMARY KEY,
    private_key text,
    created_at timestamp
);
