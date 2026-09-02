-- The provenance check must see the provenance (WP-H4).
--
-- knowledge_record_requires_provenance ran with invoker rights, so its
-- EXISTS against knowledge_record_sources was filtered by row-level security.
-- Whether a record passed the check depended on who was writing.
--
-- It held while only the reviewer wrote to knowledge_records: their identity is
-- set, so RLS shows them their own project's provenance. The publisher writes
-- as the system with no app user, so RLS showed it nothing and the trigger
-- refused a record whose provenance was sitting in the table:
--
--   ERROR: knowledge record e50a7c49-... has no source and is not
--   human_authored
--
-- Two pull requests were open in company-workgraph at the time, for the very
-- records it said had no source.
--
-- An integrity check has to see the truth rather than the writer's slice of it.
-- Under RLS this one can only fail wrongly -- hidden rows make it stricter, not
-- looser -- but "stricter" here means a correct record is refused depending on
-- who saves it, which is not an integrity rule anybody can reason about.
BEGIN;

CREATE OR REPLACE FUNCTION knowledge_record_requires_provenance() RETURNS trigger
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
BEGIN
    IF NEW.human_authored THEN
        RETURN NEW;   -- provenance is the named author
    END IF;
    IF NOT EXISTS (SELECT 1 FROM knowledge_record_sources WHERE record_id = NEW.id) THEN
        RAISE EXCEPTION
            'knowledge record % has no source and is not human_authored', NEW.id;
    END IF;
    RETURN NEW;
END;
$$;

-- The trigger itself is unchanged: same name, same timing, same function.
-- Replacing the function body is enough, and recreating the trigger would drop
-- and re-add a constraint the table is relying on.

COMMIT;
