CREATE TABLE processed_events (
    consumer        text        NOT NULL,
    event_id        bigint      NOT NULL,
    processed_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (consumer, event_id)
);

CREATE INDEX idx_processed_events_processed_at
    ON processed_events (processed_at);