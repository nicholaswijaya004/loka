CREATE TABLE bookings (
    booking_id            uuid          PRIMARY KEY DEFAULT gen_random_uuid(),
    unit_id               uuid          NOT NULL,
    customer_id           uuid          NOT NULL,
    qty                   int           NOT NULL,
    visit_date_time       timestamptz   NOT NULL,
    total_minor           bigint        NOT NULL,
    currency              varchar(3)    NOT NULL,
    booking_status        text          NOT NULL,
    failure_reason        text, 
    confirmed_at          timestamptz,
    cancelled_at          timestamptz, 
    created_at            timestamptz NOT NULL DEFAULT now(),
    updated_at            timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT fk_bookings_unit
        FOREIGN KEY (unit_id) REFERENCES inventory_units(unit_id) ON DELETE RESTRICT,
    CONSTRAINT fk_bookings_customer
        FOREIGN KEY (customer_id) REFERENCES customers(customer_id) ON DELETE RESTRICT,
    CONSTRAINT chk_booking_status
        CHECK (booking_status IN ('pending','confirmed','cancelled')),
    CONSTRAINT chk_not_both_states
        CHECK (NOT (confirmed_at IS NOT NULL AND cancelled_at IS NOT NULL)),
    CONSTRAINT chk_qty_positive
        CHECK (qty > 0),
    CONSTRAINT chk_total_zero_or_positive
        CHECK (total_minor >= 0)
);

CREATE INDEX idx_bookings_unit ON bookings (unit_id);
CREATE INDEX idx_bookings_customer ON bookings (customer_id);