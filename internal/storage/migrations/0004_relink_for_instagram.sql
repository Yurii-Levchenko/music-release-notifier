-- Adding a new kind of link makes every previous lookup incomplete.
--
-- links_fetched_at exists to stop an endless re-fetch of artists who have no
-- links (0003). The cost of that is exactly this: an artist looked up before
-- Instagram was collected is marked done and will never be looked at again, so
-- the new field would only ever appear for artists subscribed to from now on.
-- The same half-a-feature problem the backfill was written to solve.
--
-- So clear the marker for rows that predate the Instagram key and let the
-- poller's backfill re-resolve them. Rows that keep coming back without an
-- instagram key simply have none: this migration runs once, so there is no
-- loop — they are marked done again and stay done.
--
-- The alternative was a version number stored next to the links. Rejected: a
-- one-line migration when the set of kinds changes is explicit and visible in
-- the schema history, and a version field is one more thing that can silently
-- disagree with the code that reads it.
UPDATE artists
SET links_fetched_at = NULL
WHERE NOT (links ? 'instagram');
