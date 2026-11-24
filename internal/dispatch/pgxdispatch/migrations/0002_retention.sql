-- Retention: when a task ended, and when an event was appended.

-- Set by the dispatcher when a task becomes done or failed. Rows a build
-- before this one finishes get it from the next build's pruner (a backfill).
ALTER TABLE tasks ADD COLUMN ended_at timestamptz;
UPDATE tasks SET ended_at = clock_timestamp() WHERE state IN ('done', 'failed');
CREATE INDEX tasks_ended ON tasks (ended_at) WHERE state IN ('done', 'failed');

-- now() is stable, so existing rows get the migration's time without a
-- rewrite of the table. Event.time is the emitter's and can be anything.
ALTER TABLE events ADD COLUMN added timestamptz NOT NULL DEFAULT now();
CREATE INDEX events_added ON events (added);
CREATE INDEX events_subject ON events (subject);
