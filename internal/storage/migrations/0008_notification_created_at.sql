-- 0008 — when a notification was queued, so the queue's age is the wait a
-- person actually has.
--
-- The age used to be measured from min(next_attempt_at), which is how overdue
-- the next attempt is, not how long anybody has been waiting. The two part
-- ways exactly when it matters: during a Telegram outage every row is
-- rescheduled into the future, the "age" reads zero, and the alert that exists
-- for a stuck queue stays quiet for the whole outage (review 16.09,
-- Resilience #7).
--
-- Existing rows get the time of this migration. They are all delivered
-- already; what matters is that every new row carries the real moment.
ALTER TABLE notifications
    ADD COLUMN created_at TIMESTAMPTZ NOT NULL DEFAULT now();
