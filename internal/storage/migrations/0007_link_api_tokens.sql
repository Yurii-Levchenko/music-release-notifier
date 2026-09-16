-- 0007 — the API credential is issued by the server, not chosen by the client.
--
-- SPEC contradicted itself here. §7 says the bearer is "видається при
-- лінкуванні" — issued at linking — while §9.3 has the extension generating
-- install_id and using that as the bearer. Only one of those is safe.
--
-- A client-chosen bearer cannot be verified. The server has no way to tell 32
-- random bytes from the literal string "test", so one extension build with a
-- hardcoded or low-entropy id would put every one of its users into the same
-- account, and anyone who learned another install's id would hold their
-- credential. install_id keeps its job — naming one extension install across
-- restarts — and stops being a secret.
--
-- The token is minted at /link/init and handed to the extension immediately,
-- before anyone has linked anything. That is deliberate and safe: until a
-- Telegram user redeems the matching link token, it authenticates nothing, so
-- the extension can simply poll /v1/me and watch a 401 turn into a 200. It
-- removes the separate "poll for your credential" round trip that RFC 8628's
-- device flow needs, while keeping the part that matters — the credential is
-- server-generated.
--
-- Only hashes are stored, like link_tokens already does for its own token: a
-- database dump must not be a set of working credentials.

ALTER TABLE link_tokens
    ADD COLUMN api_token_hash BYTEA NOT NULL;

ALTER TABLE installs
    ADD COLUMN api_token_hash BYTEA NOT NULL,
    ADD CONSTRAINT installs_api_token_hash_key UNIQUE (api_token_hash);

-- Bearer lookups hit this on every authenticated request. UNIQUE already
-- indexes it, so nothing more is needed here — the comment exists so the next
-- person does not add a duplicate index for the same access path.
