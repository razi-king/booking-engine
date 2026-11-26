-- Schema for the booking engine. Every statement is "IF NOT EXISTS", so the
-- server can run this file on every start-up safely (an idempotent migration).

CREATE TABLE IF NOT EXISTS flights (
    id          BIGSERIAL PRIMARY KEY,          -- auto-incrementing id
    number      TEXT        NOT NULL,
    origin      TEXT        NOT NULL,
    destination TEXT        NOT NULL,
    departure   TIMESTAMPTZ NOT NULL,           -- "with time zone": stored as UTC, never ambiguous
    seat_rows   INT         NOT NULL CHECK (seat_rows > 0),
    UNIQUE (number, departure)                   -- one row per departure of a flight number
);

CREATE TABLE IF NOT EXISTS holds (
    id          TEXT        PRIMARY KEY,
    flight_id   BIGINT      NOT NULL REFERENCES flights(id),
    user_id     TEXT        NOT NULL,
    seats       TEXT[]      NOT NULL,           -- Postgres arrays: the hold's seats in seat order
    total_paise BIGINT      NOT NULL CHECK (total_paise >= 0),
    status      TEXT        NOT NULL CHECK (status IN ('active','confirmed','released','expired')),
    expires_at  TIMESTAMPTZ NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL
);

-- PARTIAL INDEX: only active holds are ever searched by expiry, and they are a
-- tiny fraction of all holds. Indexing just those keeps the index small and
-- makes "find active holds that have expired" a quick B-tree range scan.
CREATE INDEX IF NOT EXISTS holds_active_expiry ON holds (expires_at) WHERE status = 'active';

CREATE TABLE IF NOT EXISTS bookings (
    pnr          TEXT        PRIMARY KEY,       -- a duplicate PNR is impossible: the DB rejects it
    hold_id      TEXT        NOT NULL UNIQUE REFERENCES holds(id), -- one booking per hold, ever
    flight_id    BIGINT      NOT NULL REFERENCES flights(id),
    user_id      TEXT        NOT NULL,
    seats        TEXT[]      NOT NULL,
    total_paise  BIGINT      NOT NULL,
    status       TEXT        NOT NULL CHECK (status IN ('confirmed','cancelled')),
    created_at   TIMESTAMPTZ NOT NULL,
    cancelled_at TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS seats (
    flight_id      BIGINT  NOT NULL REFERENCES flights(id),
    seat_no        TEXT    NOT NULL,            -- "12A"
    seat_row       INT     NOT NULL,            -- 12   } stored separately so ORDER BY sorts
    seat_letter    TEXT    NOT NULL,            -- "A"  } by row NUMBER (the lock order)
    cabin          TEXT    NOT NULL,
    price_paise    BIGINT  NOT NULL CHECK (price_paise > 0),
    hold_id        TEXT    REFERENCES holds(id),   -- NULL = no hold
    hold_until     TIMESTAMPTZ,                    -- copy of the hold's deadline: no JOIN needed
    booking_pnr    TEXT    REFERENCES bookings(pnr),-- NULL = not booked
    PRIMARY KEY (flight_id, seat_row, seat_letter),
    UNIQUE (flight_id, seat_no)
);

CREATE INDEX IF NOT EXISTS seats_hold ON seats (hold_id) WHERE hold_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS seats_booking ON seats (booking_pnr) WHERE booking_pnr IS NOT NULL;

CREATE TABLE IF NOT EXISTS idempotency_keys (
    key          TEXT        PRIMARY KEY,
    fingerprint  TEXT        NOT NULL,          -- SHA-256 of the request
    status_code  INT,                           -- NULL while the first request is running
    body         BYTEA,
    created_at   TIMESTAMPTZ NOT NULL
);
