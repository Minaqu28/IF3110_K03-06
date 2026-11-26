ALTER TABLE groups
    ADD COLUMN leader_id BIGINT REFERENCES users (id) ON DELETE SET NULL;

CREATE INDEX groups_leader_id_idx ON groups (leader_id) WHERE leader_id IS NOT NULL;
