ALTER TABLE sessions ADD COLUMN previous_valid_until TIMESTAMPTZ;

-- Previous identifiers issued before this migration had no explicit expiry.
UPDATE sessions SET previous_hash = NULL;

ALTER TABLE sessions ADD CONSTRAINT sessions_previous_expiry_pair
    CHECK ((previous_hash IS NULL) = (previous_valid_until IS NULL));
