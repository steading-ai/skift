-- Extends the events partitions to the horizon the service keeps.
--
-- 00007 created the partition for the month it ran in and the next one,
-- and nothing created more, so every release through v0.3.0 stopped
-- accepting writes once the clock passed the second month after its first
-- migration. The service now keeps the months ahead in place (the
-- ensure-event-partitions job, and a pass when serve starts); this
-- migration makes the upgrade itself enough, before either has run. The
-- 3 is admin.EventPartitionsAhead.

-- +goose Up
SELECT ensure_event_partitions(now(), 3);

-- +goose Down
-- Partitions are kept by the service as time passes, not by any one
-- migration, and one may already hold events, so going down leaves them.
SELECT 1;
