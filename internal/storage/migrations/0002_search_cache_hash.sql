-- 0002 — give artist_search_cache a short stable handle.
--
-- Telegram caps callback_data at 64 BYTES (SPEC.md C6), so a picker button
-- cannot carry the search query it belongs to. It carries a hash instead, and
-- the bot looks the candidate list back up by that hash.
--
-- 12 hex characters is 48 bits. A button payload of "s:<12 hex>:<index>" is
-- ~17 bytes, well inside the cap, and collisions are negligible for a cache
-- that expires after 7 days. Reads still verify the stored query text, so a
-- collision degrades to a cache miss rather than showing the wrong artists.

ALTER TABLE artist_search_cache
    ADD COLUMN query_hash TEXT;

-- sha256() on bytea is built in from PostgreSQL 11; no pgcrypto needed.
UPDATE artist_search_cache
SET query_hash = substring(encode(sha256(query::bytea), 'hex') FOR 12)
WHERE query_hash IS NULL;

ALTER TABLE artist_search_cache
    ALTER COLUMN query_hash SET NOT NULL;

-- Unique so a collision surfaces as a conflict we handle, not as two rows
-- racing to answer the same button.
CREATE UNIQUE INDEX artist_search_cache_query_hash_idx
    ON artist_search_cache (query_hash);

-- Lets the eviction sweep find stale rows without a full scan.
CREATE INDEX artist_search_cache_fetched_at_idx
    ON artist_search_cache (fetched_at);
