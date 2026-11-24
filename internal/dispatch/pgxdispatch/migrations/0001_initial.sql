-- The dispatcher's tables. Run at every start; every statement is idempotent.

-- One row. Every write transaction locks it first, so writes are serialized:
-- events are numbered and committed in order, and transitions never race.
CREATE TABLE IF NOT EXISTS meta (
    one    boolean PRIMARY KEY DEFAULT true CHECK (one),
    seq    bigint NOT NULL DEFAULT 0, -- the last Seq given to an event
    cursor bigint NOT NULL DEFAULT 0  -- events up to this Seq have been acted on
);
INSERT INTO meta DEFAULT VALUES ON CONFLICT DO NOTHING;

-- The log. json, not jsonb, so that data comes back byte for byte.
CREATE TABLE IF NOT EXISTS events (
    seq     bigint PRIMARY KEY,
    id      text NOT NULL UNIQUE,
    type    text NOT NULL,
    source  text NOT NULL,
    subject text NOT NULL DEFAULT '',
    time    timestamptz NOT NULL,
    data    json
);
CREATE INDEX IF NOT EXISTS events_type_subject ON events (type, subject);

CREATE TABLE IF NOT EXISTS tasks (
    id            text PRIMARY KEY,
    spec          json NOT NULL,   -- the TaskSpec without its payload
    payload       json,
    priority      integer NOT NULL,
    ord           bigint NOT NULL, -- submission order
    timeout_us    bigint NOT NULL,
    start_by      timestamptz,
    state         text NOT NULL CHECK (state IN ('pending', 'running', 'waiting', 'done', 'failed')),
    attempt       integer NOT NULL DEFAULT 0, -- the current or next attempt
    started_at    timestamptz,     -- of the current attempt, when running
    wait          json,            -- the Wait, when waiting
    wait_type     text,
    wait_subject  text,
    wait_deadline timestamptz,
    wait_after    bigint,          -- Seq of the task.finished that asked for the wait
    wake          json,            -- the Wake for the next attempt
    not_started   boolean NOT NULL DEFAULT false, -- task.not-started was emitted
    starting_by   bigint,          -- the instance whose start call is in progress
    reason        text NOT NULL DEFAULT '',
    reason_detail text NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS tasks_open ON tasks (state, priority DESC, ord)
    WHERE state IN ('pending', 'running', 'waiting');
CREATE INDEX IF NOT EXISTS tasks_wait ON tasks (wait_type, wait_subject) WHERE state = 'waiting';

CREATE TABLE IF NOT EXISTS schedules (
    name       text PRIMARY KEY,
    spec       json NOT NULL,      -- the Schedule
    interval_s bigint NOT NULL,
    last_tick  timestamptz NOT NULL
);

-- Running instances. A start claim (tasks.starting_by) counts while its
-- instance's lease has not run out.
CREATE TABLE IF NOT EXISTS instances (
    id          bigserial PRIMARY KEY,
    build       text NOT NULL,
    opened      timestamptz NOT NULL DEFAULT clock_timestamp(),
    alive_until timestamptz NOT NULL
);
