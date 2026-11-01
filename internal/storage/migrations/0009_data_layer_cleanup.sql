-- 0009 — data-layer fixes from the 16.09 review (Data #2, #6, #9).
--
-- 1. releases.dedup_key is no longer UNIQUE.
--
-- The release group is the dedup key (SPEC D7), and the insert says so:
-- ON CONFLICT (release_group_mbid) DO NOTHING. But dedup_key carried a UNIQUE
-- of its own, and ON CONFLICT only absorbs the constraint it names. Two
-- different release groups that normalize to the same artist|title|date — a
-- title-track single released the same day as its album, "Live" and "Live!" —
-- raised 23505 instead: the second release was never recorded and never
-- announced, and the poller logged the same error every day for a week.
--
-- Dropping the rule rather than folding it into ON CONFLICT, because skipping
-- the second release silently is the same loss without the log line. The cost
-- is that a release group MusicBrainz holds twice by mistake is announced
-- twice until their editors merge it. Twice is better than never. The column
-- stays: nothing reads it today, and a second release source (UPC matching)
-- may want it.
ALTER TABLE releases DROP CONSTRAINT releases_dedup_key_key;

-- 2. One live link code per install is a rule of the database.
--
-- CreateLinkToken used to delete the install's previous code and then insert a
-- new one, as two statements. Two /link/init calls racing for one install could
-- both commit and leave two working codes, and redeeming the one the extension
-- had already replaced bound an api token the extension no longer held — a 401
-- until somebody linked again. Uniqueness belongs to the schema, not to Go
-- (CLAUDE.md); CreateLinkToken now upserts against this index in one statement.
--
-- A database restored from before this rule may hold several live codes for one
-- install. Keep the newest: it is the one the extension showed last.
DELETE FROM link_tokens old
WHERE old.redeemed_at IS NULL
  AND EXISTS (
      SELECT 1
      FROM link_tokens newer
      WHERE newer.install_id = old.install_id
        AND newer.redeemed_at IS NULL
        AND (newer.created_at, newer.token_hash) > (old.created_at, old.token_hash)
  );

CREATE UNIQUE INDEX link_tokens_live_install_idx
    ON link_tokens (install_id)
    WHERE redeemed_at IS NULL;

-- 3. A duplicate index.
--
-- spotify_id is already UNIQUE, and that constraint is an index on the same
-- column. This one was never scanned (idx_scan = 0) and only cost writes.
DROP INDEX artists_spotify_id_idx;
