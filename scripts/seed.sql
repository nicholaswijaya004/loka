INSERT INTO customers (customer_id, name, email, phone)
VALUES ('11111111-1111-1111-1111-111111111111', 'Test Customer', 'test@example.com', '+628123456789')
ON CONFLICT (customer_id) DO NOTHING;

INSERT INTO inventory_units (unit_id, name, description, available_units, total_units, currency, price_minor, min_book)
VALUES
    ('22222222-2222-2222-2222-222222222222', 'Deluxe Cabin', 'Sea view, king bed', 10, 10, 'IDR', 150000000, 1),
    ('33333333-3333-3333-3333-333333333333', 'Standard Cabin', 'Interior, twin beds', 50, 50, 'IDR', 75000000, 1),
    ('44444444-4444-4444-4444-444444444444', 'Single Seat', 'Contention test unit', 1, 1, 'IDR', 50000000, 1)
ON CONFLICT (unit_id) DO NOTHING;