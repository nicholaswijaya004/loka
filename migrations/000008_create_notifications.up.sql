CREATE TABLE notifications (
    notification_id  uuid          PRIMARY KEY DEFAULT gen_random_uuid(),
    booking_id       uuid          NOT NULL,
    customer_id      uuid          NOT NULL,
    kind             text          NOT NULL,
    event_id         bigint        NOT NULL,
    created_at       timestamptz   NOT NULL DEFAULT now(),
    sent_at          timestamptz,

    CONSTRAINT chk_notification_kind
        CHECK (kind IN ('booking_confirmation'))
);

CREATE UNIQUE INDEX uq_notifications_booking_kind
    ON notifications (booking_id, kind);