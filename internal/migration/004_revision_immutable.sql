CREATE FUNCTION forbid_locked_revision_change() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        IF OLD.locked_at IS NOT NULL THEN
            RAISE EXCEPTION 'migration revision % is locked', OLD.id USING ERRCODE = 'check_violation';
        END IF;
        RETURN OLD;
    END IF;
    IF OLD.locked_at IS NOT NULL AND (
        NEW.migration_id, NEW.database_id, NEW.revision, NEW.up_sql, NEW.down_sql, NEW.reversible,
        NEW.definition_hash, NEW.batch_table, NEW.batch_cursor_column, NEW.batch_size, NEW.locked_at
    ) IS DISTINCT FROM (
        OLD.migration_id, OLD.database_id, OLD.revision, OLD.up_sql, OLD.down_sql, OLD.reversible,
        OLD.definition_hash, OLD.batch_table, OLD.batch_cursor_column, OLD.batch_size, OLD.locked_at
    ) THEN
        RAISE EXCEPTION 'migration revision % is locked', OLD.id USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END
$$;

CREATE TRIGGER migration_revisions_immutable
    BEFORE UPDATE OR DELETE ON migration_revisions
    FOR EACH ROW EXECUTE FUNCTION forbid_locked_revision_change();
