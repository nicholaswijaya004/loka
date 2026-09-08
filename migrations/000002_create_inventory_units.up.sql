CREATE TABLE inventory_units (
    unit_id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    name               text        NOT NULL,
    description        text,
    available_units    int         NOT NULL,
    total_units        int         NOT NULL,
    currency           varchar(3)  NOT NULL,
    price_minor        bigint      NOT NULL,
    min_book           int         NOT NULL DEFAULT 1, 
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now(),
    version            int         NOT NULL DEFAULT 0,
    CONSTRAINT chk_availability
        CHECK (available_units >= 0 AND available_units <= total_units)
);