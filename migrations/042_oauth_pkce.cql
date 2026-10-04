-- PKCE (RFC 7636): a public client (SPA, mobile app) that cannot keep a client secret sends a
-- code_challenge when asking for consent and proves the matching code_verifier at the token
-- endpoint, so an authorization code intercepted on the redirect is useless on its own.
-- Optional per request; only the S256 method is accepted.
ALTER TABLE oauth_codes ADD code_challenge text;
ALTER TABLE oauth_codes ADD code_challenge_method text;
