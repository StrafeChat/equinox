-- Admin-managed federation allow/block list, edited live from the instance dashboard and
-- merged with the static FEDERATION_ALLOWLIST/FEDERATION_BLOCKLIST env lists by IsAllowedPeer.
-- Single partition (bucket is always 0): the list is small and read whole on load.
CREATE TABLE IF NOT EXISTS federation_peer_policy (
  bucket int,
  domain text,
  kind text,          -- 'allow' | 'block'
  added_by bigint,    -- instance admin who added the entry
  created_at timestamp,
  PRIMARY KEY ((bucket), domain)
);
