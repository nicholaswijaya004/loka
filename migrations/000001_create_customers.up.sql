CREATE TABLE customers (
    customer_id  uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    name         text        NOT NULL,
    email        text        NOT NULL UNIQUE,
    phone        text,
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now()
);