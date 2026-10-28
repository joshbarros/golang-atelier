CREATE TABLE workshops.workshops (
    id                uuid PRIMARY KEY,
    code              text        NOT NULL UNIQUE CHECK (char_length(code) <= 20),
    name              text        NOT NULL,
    track            text        NOT NULL,
    instructor            text        NOT NULL,
    workshop_date      timestamptz NOT NULL,
    price_cents       bigint      NOT NULL,
    seats_total       integer     NOT NULL,
    seats_left        integer     NOT NULL,
    -- laptop kits to lend, one per seat that asks for one; taken together with the seats or not at all
    kits_total        integer     NOT NULL DEFAULT 0,
    kits_left         integer     NOT NULL DEFAULT 0,
    kit_price_cents   bigint      NOT NULL DEFAULT 0,
    max_per_attendee integer     NOT NULL,
    created_at        timestamptz NOT NULL,
    CONSTRAINT workshops_remaining CHECK (seats_left >= 0 AND seats_left <= seats_total),
    CONSTRAINT workshops_kits CHECK (kits_left >= 0 AND kits_left <= kits_total),
    CONSTRAINT workshops_cap CHECK (max_per_attendee >= 1 AND max_per_attendee <= seats_total)
);

-- How many seats one attendee holds in one workshop. Written only by the reserve and release handlers.
CREATE TABLE workshops.attendees (
    workshop_id  uuid    NOT NULL REFERENCES workshops.workshops (id),
    owner_id uuid    NOT NULL,
    seats    integer NOT NULL CHECK (seats >= 0),
    PRIMARY KEY (workshop_id, owner_id)
);

CREATE TABLE workshops.seat_holds (
    order_id       uuid PRIMARY KEY,
    owner_id       uuid        NOT NULL,
    workshop_id        uuid        NOT NULL REFERENCES workshops.workshops (id),
    seats          integer     NOT NULL,
    kits           integer     NOT NULL DEFAULT 0,
    status         text        NOT NULL,
    created_at     timestamptz NOT NULL,
    released_at    timestamptz,
    release_reason text
);

CREATE TABLE workshops.outbox_messages (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    message_id  uuid        NOT NULL,
    module      text        NOT NULL,
    routing_key text        NOT NULL,
    key         text        NOT NULL,
    payload     jsonb       NOT NULL,
    created_at  timestamptz NOT NULL,
    sent_at     timestamptz
);
CREATE INDEX workshops_outbox_unsent ON workshops.outbox_messages (id) WHERE sent_at IS NULL;

CREATE TABLE workshops.inbox_messages (
    consumer   text        NOT NULL,
    message_id uuid        NOT NULL,
    claimed_at timestamptz NOT NULL,
    PRIMARY KEY (consumer, message_id)
);
