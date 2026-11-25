-- Where the service's own readers have got to, outside the contract: the
-- notices' place in the log, and what has been judged of each CronJob's
-- ticks. One JSON value per name, so that a restart carries on from there.
CREATE TABLE IF NOT EXISTS marks (
    name  text PRIMARY KEY,
    value json NOT NULL
);
