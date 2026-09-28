ALTER TABLE bookings
    ADD COLUMN next_attempt_at timestamptz NOT NULL DEFAULT now();

CREATE INDEX idx_bookings_payment_due
    ON bookings (next_attempt_at)
    WHERE booking_status = 'payment_pending';