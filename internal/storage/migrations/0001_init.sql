-- 0001_init — full schema per SPEC.md §6.
--
-- The two UNIQUE constraints below are load-bearing. They are the whole
-- deduplication design (NFR-1, NFR-2): correctness lives here, not in Go.
-- Never replace them with a read-then-write check in application code.

-- A user is one delivery identity. telegram_chat_id is NUMERIC — a bot cannot
-- send to an @username (SPEC C1), so the username is display-only.
CREATE TABLE users (
    id                BIGSERIAL PRIMARY KEY,
    telegram_chat_id  BIGINT      NOT NULL UNIQUE,
    telegram_username TEXT,
    locale            TEXT        NOT NULL DEFAULT 'uk',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    blocked_at        TIMESTAMPTZ
);

-- Artists are keyed by MusicBrainz MBID, not by Spotify id. spotify_id stays
-- NULL until the v2 extension resolves it (SPEC §7).
CREATE TABLE artists (
    mbid        UUID        PRIMARY KEY,
    name        TEXT        NOT NULL,
    spotify_id  TEXT        UNIQUE,
    resolved_at TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX artists_spotify_id_idx ON artists (spotify_id)
    WHERE spotify_id IS NOT NULL;

-- NFR-2: a user/artist pair cannot duplicate. Subscribing from the bot and
-- from the extension collapses to one row by construction.
CREATE TABLE subscriptions (
    user_id     BIGINT      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    artist_mbid UUID        NOT NULL REFERENCES artists (mbid) ON DELETE CASCADE,
    source      TEXT        NOT NULL CHECK (source IN ('bot', 'extension')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, artist_mbid)
);

CREATE INDEX subscriptions_artist_idx ON subscriptions (artist_mbid);

-- release_group_mbid is the dedup key (SPEC D7): MusicBrainz release *groups*
-- already merge every edition of one album, which Spotify's album ids do not.
-- dedup_key is a cheap fallback for sources that lack a release group.
CREATE TABLE releases (
    id                 BIGSERIAL   PRIMARY KEY,
    release_group_mbid UUID        NOT NULL UNIQUE,
    artist_mbid        UUID        NOT NULL REFERENCES artists (mbid),
    title              TEXT        NOT NULL,
    primary_type       TEXT        NOT NULL CHECK (primary_type IN ('Album', 'Single', 'EP')),
    release_date       DATE        NOT NULL,
    -- Built by the poller from caa_id + caa_release_mbid in the poll response.
    -- Costs zero HTTP calls (SPEC C27, D14). Empty for ~26% of releases (C27b).
    cover_url          TEXT,
    dedup_key          TEXT        NOT NULL UNIQUE,
    first_seen_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX releases_artist_idx ON releases (artist_mbid);

-- Outbox. NFR-1 + NFR-3: state lives here, never in process memory, so a
-- restart neither loses nor duplicates a notification.
CREATE TABLE notifications (
    id              BIGSERIAL   PRIMARY KEY,
    user_id         BIGINT      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    release_id      BIGINT      NOT NULL REFERENCES releases (id) ON DELETE CASCADE,
    state           TEXT        NOT NULL DEFAULT 'pending'
                                CHECK (state IN ('pending', 'sent', 'failed', 'skipped')),
    attempts        INT         NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_error      TEXT,
    sent_at         TIMESTAMPTZ,
    -- NFR-1: the "exactly one message per release" guarantee.
    UNIQUE (user_id, release_id)
);

-- Supports the notifier's claim query:
--   SELECT ... WHERE state='pending' AND next_attempt_at <= now()
--   ORDER BY next_attempt_at FOR UPDATE SKIP LOCKED
CREATE INDEX notifications_claim_idx ON notifications (next_attempt_at)
    WHERE state = 'pending';

-- Account linking (SPEC D6). Only the hash is stored; short_code is the manual
-- fallback because ?start= payloads are unreliable on repeat use (C4).
CREATE TABLE link_tokens (
    token_hash  BYTEA       PRIMARY KEY,
    short_code  TEXT        NOT NULL UNIQUE,
    install_id  TEXT        NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at  TIMESTAMPTZ NOT NULL,
    redeemed_at TIMESTAMPTZ,
    user_id     BIGINT      REFERENCES users (id) ON DELETE CASCADE
);

CREATE INDEX link_tokens_expires_idx ON link_tokens (expires_at)
    WHERE redeemed_at IS NULL;

-- Binds one extension install to a user; install_id doubles as the API bearer.
CREATE TABLE installs (
    install_id   TEXT        PRIMARY KEY,
    user_id      BIGINT      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    linked_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ
);

-- MusicBrainz allows 1 req/s per IP (SPEC C25), so repeated searches are cached.
CREATE TABLE artist_search_cache (
    query      TEXT        PRIMARY KEY,
    payload    JSONB       NOT NULL,
    fetched_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Lets the poller survive a restart without re-walking a window it finished.
CREATE TABLE poll_state (
    source         TEXT PRIMARY KEY,
    last_polled_at TIMESTAMPTZ,
    last_ok_at     TIMESTAMPTZ,
    last_error     TEXT
);

-- D13: the delivery channel is a plugin. Only 'telegram' is implemented in v1,
-- but the boundary exists from the first commit so email costs an evening
-- instead of a rewrite. users.telegram_chat_id migrates in here when a second
-- channel arrives; the domain will not notice.
CREATE TABLE channels (
    id          BIGSERIAL   PRIMARY KEY,
    user_id     BIGINT      NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    kind        TEXT        NOT NULL CHECK (kind IN ('telegram', 'email', 'whatsapp')),
    address     TEXT        NOT NULL,
    verified_at TIMESTAMPTZ,
    disabled_at TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (user_id, kind, address)
);

CREATE INDEX channels_user_idx ON channels (user_id) WHERE disabled_at IS NULL;
