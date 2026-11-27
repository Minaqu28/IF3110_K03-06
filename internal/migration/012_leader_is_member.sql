INSERT INTO group_members (group_id, user_id)
SELECT id, leader_id FROM groups WHERE leader_id IS NOT NULL
ON CONFLICT DO NOTHING;

ALTER TABLE groups
    ADD CONSTRAINT groups_leader_is_member
    FOREIGN KEY (id, leader_id) REFERENCES group_members (group_id, user_id)
    ON DELETE SET NULL (leader_id);
