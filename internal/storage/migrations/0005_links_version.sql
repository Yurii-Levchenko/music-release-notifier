-- Which set of link kinds a row was resolved against.
--
-- 0003 introduced links_fetched_at to stop re-fetching artists who have none.
-- 0004 then had to clear it by hand so a newly collected kind (Instagram)
-- would be picked up. That approach failed the first time it was used: the
-- migration ran during the deploy that introduced the field, before the code
-- writing it was correct, and burned its one-time effect on nothing. The rows
-- were marked resolved and the new field never appeared.
--
-- The obvious alternative — "re-resolve anyone missing an instagram key" —
-- cannot work either, because an artist who has no Instagram is
-- indistinguishable from one resolved before we looked for it, so it would
-- re-fetch those artists forever.
--
-- A version number distinguishes the two. Bumping a constant in the code is
-- enough for the backfill to pick everyone up, once, with no migration and no
-- false re-fetching. Starting at 0 so every existing row is stale by
-- definition.
ALTER TABLE artists ADD COLUMN links_version INT NOT NULL DEFAULT 0;

-- The backfill's query: never resolved, or resolved against an older set.
DROP INDEX IF EXISTS artists_links_pending_idx;
CREATE INDEX artists_links_pending_idx ON artists (links_version, mbid);
