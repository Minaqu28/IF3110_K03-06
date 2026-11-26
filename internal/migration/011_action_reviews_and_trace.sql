ALTER TABLE review_requests
    ADD COLUMN action TEXT NOT NULL DEFAULT 'APPLY',
    ADD COLUMN target_execution_id UUID REFERENCES migration_executions (id) ON DELETE RESTRICT;

ALTER TABLE review_requests
    ADD CONSTRAINT review_requests_action CHECK (action IN ('APPLY', 'ROLLBACK', 'FORWARD')),
    ADD CONSTRAINT review_requests_target_only_for_rollback CHECK (action = 'ROLLBACK' OR target_execution_id IS NULL);

ALTER TABLE migration_jobs
    ADD COLUMN trace_context JSONB,
    ADD CONSTRAINT migration_jobs_trace_context_object CHECK (trace_context IS NULL OR jsonb_typeof(trace_context) = 'object');
