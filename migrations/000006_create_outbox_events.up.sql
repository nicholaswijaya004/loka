CREATE TABLE outbox_events (
    id                bigserial     PRIMARY KEY,
    aggregate_type    text          NOT NULL,
    aggregate_id      uuid          NOT NULL,
    event_type        text          NOT NULL,
    payload           jsonb         NOT NULL,
    attempt_number    int           NOT NULL DEFAULT 0,
    error             text,
    created_at        timestamptz   NOT NULL DEFAULT now(),
    published_at      timestamptz
);

CREATE INDEX idx_outbox_unpublished
ON outbox_events (id)
WHERE published_at IS NULL;