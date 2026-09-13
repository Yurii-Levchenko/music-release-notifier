-- Why a notification was queued.
--
-- 'release' is the normal path: the poller saw something new and fanned it out
-- to everyone subscribed at that moment.
--
-- 'catch_up' is the one queued when somebody subscribes to an artist who put
-- something out in the last few days. Same delivery path, same dedup, but the
-- message has to say "recent release" rather than "new release" — the release
-- is days old and the reader knows it. Calling it new would be the kind of
-- small lie that makes somebody stop trusting the rest of the message.
ALTER TABLE notifications
    ADD COLUMN kind TEXT NOT NULL DEFAULT 'release'
        CHECK (kind IN ('release', 'catch_up'));
