CREATE TABLE idempotency_keys (
    idempotency_key    text        PRIMARY KEY,
    booking_id         uuid,
    request_hash       text        NOT NULL,
    response_status    int,
    response_body      jsonb,
    state              text        NOT NULL,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    expires_at         timestamptz,
    CONSTRAINT fk_idempotency_keys_booking
        FOREIGN KEY (booking_id) REFERENCES bookings(booking_id) ON DELETE RESTRICT,
    CONSTRAINT chk_idempotency_state
        CHECK (state IN ('in_progress','completed','failed'))
);

CREATE INDEX idx_idempotency_keys_unique_per_expiration
ON idempotency_keys (expires_at)
WHERE state = 'completed';