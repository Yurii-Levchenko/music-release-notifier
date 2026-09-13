-- External links for an artist: where to actually go and listen.
--
-- Until now a release notification linked only to MusicBrainz, which is a
-- metadata site. Somebody who just learned their artist released an album
-- wants to press play, not read a database entry.
--
-- Stored as jsonb rather than a column per service. The set of links we show
-- is small and curated by us, but which services those are is a product
-- decision that will change (bandcamp, soundcloud), and a migration per
-- streaming service is a poor trade for the type safety gained on three
-- strings.
ALTER TABLE artists ADD COLUMN links JSONB NOT NULL DEFAULT '{}'::jsonb;

-- Separate from links itself, because '{}' has two meanings that must not be
-- confused: "we have never looked" and "we looked and this artist has none".
-- Without this column the second case would be retried on every subscribe,
-- forever — the same mistake as defining the poller's first run as "the
-- releases table is empty" (see SPEC §16, 30.08.2026).
ALTER TABLE artists ADD COLUMN links_fetched_at TIMESTAMPTZ;

-- Supports "which tracked artists still need a lookup", which is how a
-- backfill would find its work.
CREATE INDEX artists_links_pending_idx ON artists (mbid)
    WHERE links_fetched_at IS NULL;
