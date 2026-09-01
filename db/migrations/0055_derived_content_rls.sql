-- Row-level security for the tables that hold source-derived content and did
-- not have it.
--
-- Found by asking the database rather than by reading migrations: three tables
-- carry material derived from a knowledge source and were readable by any
-- authenticated caller. This is the third time today that a table holding
-- derived content turned out to have no policy -- event_receipts, then
-- knowledge_reviews and its neighbours, now these -- which is why
-- test/integration/derived_content_rls.sql lands with this migration rather
-- than after the next one.
BEGIN;

-- human_corrections holds before_value and after_value: the candidate statement
-- as it was and as a person corrected it, plus a mandatory reason. That is
-- source content quoted verbatim, so a correction to a restricted client's
-- candidate was readable by anyone.
--
-- A correction has two possible subjects (the CHECK requires at least one): a
-- candidate, or an agent run. Each follows its own subject rather than being
-- given a single blunt rule, because they are different kinds of thing with
-- different audiences.
ALTER TABLE human_corrections ENABLE ROW LEVEL SECURITY;
ALTER TABLE human_corrections FORCE ROW LEVEL SECURITY;

CREATE POLICY human_corrections_read ON human_corrections FOR SELECT
    USING (
        (candidate_id IS NOT NULL AND EXISTS (
            SELECT 1 FROM knowledge_candidates c
              JOIN knowledge_sources s ON s.id = c.source_id
             WHERE c.id = human_corrections.candidate_id
               AND can_read_source(s.project_id, s.visibility)))
        OR (agent_run_id IS NOT NULL AND EXISTS (
            SELECT 1 FROM agent_runs r
             WHERE r.id = human_corrections.agent_run_id))
    );

-- The agent_runs arm relies on agent_runs' own policy filtering the subquery:
-- a run the caller cannot see returns no row, so the correction is not visible
-- either. Written as EXISTS rather than a visibility function so there is one
-- definition of who may see a run, in that table's policy.

CREATE POLICY human_corrections_insert ON human_corrections FOR INSERT
    WITH CHECK (corrected_by = current_app_user());

-- knowledge_record_sources is the provenance of a durable record. Knowing which
-- sources back a record is lineage rather than content, but a restricted
-- source's id appearing against a record still says a restricted meeting fed
-- it. It follows the record, whose policy already exists.
ALTER TABLE knowledge_record_sources ENABLE ROW LEVEL SECURITY;
ALTER TABLE knowledge_record_sources FORCE ROW LEVEL SECURITY;

CREATE POLICY knowledge_record_sources_read ON knowledge_record_sources FOR SELECT
    USING (EXISTS (
        SELECT 1 FROM knowledge_records r WHERE r.id = knowledge_record_sources.record_id));

COMMIT;
