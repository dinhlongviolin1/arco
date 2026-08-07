-- SO_PEERCRED intake binding (rev7/T1.6): the UID a worker was spawned under,
-- recorded at spawn on the local VM path. Any same-box process holding the
-- intake HMAC secret could otherwise forge per-worker events over the unix
-- socket; the intake resolves the connecting peer's UID via SO_PEERCRED and
-- rejects (403 + intake_denied audit) events for a worker recorded under a
-- different UID. NULL = unknown/ungated (legacy rows, cross-VM, Fake) → the
-- intake keeps today's behavior.
ALTER TABLE workers ADD COLUMN intake_uid INTEGER;
