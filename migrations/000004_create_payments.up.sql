CREATE TABLE payments (
    payment_id            uuid          PRIMARY KEY DEFAULT gen_random_uuid(),
    booking_id            uuid          NOT NULL,
    amount_minor          bigint        NOT NULL,
    currency              varchar(3)    NOT NULL,
    payment_status        text          NOT NULL,
    failure_reason        text,
    provider_payment_id   text,
    card_last4            varchar(4),
    card_brand            text,
    succeeded_at          timestamptz,
    failed_at             timestamptz,
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT fk_payments_booking
        FOREIGN KEY (booking_id) REFERENCES bookings(booking_id),
    CONSTRAINT chk_payment_status
        CHECK (payment_status IN ('pending','succeeded','failed', 'refunded'))
);

CREATE UNIQUE INDEX idx_payments_one_success_per_booking
ON payments (booking_id)
WHERE payment_status = 'succeeded';