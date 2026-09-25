ALTER TABLE bookings
    DROP CONSTRAINT chk_booking_status,
    ADD CONSTRAINT chk_booking_status
        CHECK (booking_status IN ('pending', 'payment_pending', 'confirmed', 'cancelled'));