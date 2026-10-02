ALTER TABLE outbox_events
    DROP COLUMN trace_context;

ALTER TABLE bookings
    DROP COLUMN trace_context;