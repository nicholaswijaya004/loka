ALTER TABLE outbox_events
    ADD COLUMN trace_context JSONB;

ALTER TABLE bookings
    ADD COLUMN trace_context JSONB;