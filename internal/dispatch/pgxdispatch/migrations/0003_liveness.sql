-- An instance's liveness is its listening session as well as its lease: a
-- process that dies closes its connection, and its start claims end with it,
-- so the lease can be long and the heartbeat rare.
ALTER TABLE instances ADD COLUMN pid integer;
ALTER TABLE instances ADD COLUMN backend_start timestamptz;
