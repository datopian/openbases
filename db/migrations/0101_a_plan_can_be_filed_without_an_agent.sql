-- Filing a plan is a job kind of its own, and it runs no agent.
--
-- Planning is moving out of the platform: a person plans in their own tool and
-- files the result. That filing still has to reach the node, because a bead
-- lives in a Dolt graph on the execution host and `bd -C` takes a local path
-- -- the control plane cannot write one. So it travels the way dispatch does,
-- as a queued job the node claims.
--
-- A separate kind rather than reusing 'plan', because the two are opposites.
-- A plan job STARTS AN AGENT and spends money to decide what the work is. A
-- file job decides nothing: it carries a decision already made and writes it
-- down. Reusing the name would make "does this cost anything" unanswerable
-- from the row, and 'plan' is being removed once this replaces it.
--
-- The brief carries the plan as JSON. It is checked NOT NULL for the same
-- reason 'plan' is: a filing job with nothing to file is a job that will
-- claim a slot, run, and produce nothing.
BEGIN;

ALTER TABLE work_queue DROP CONSTRAINT IF EXISTS work_queue_kind_check;
ALTER TABLE work_queue ADD CONSTRAINT work_queue_kind_check
    CHECK (kind = ANY (ARRAY['plan'::text, 'work'::text, 'file'::text]));

ALTER TABLE work_queue DROP CONSTRAINT IF EXISTS work_queue_file_needs_brief;
ALTER TABLE work_queue ADD CONSTRAINT work_queue_file_needs_brief
    CHECK ((kind <> 'file'::text) OR (brief IS NOT NULL));

COMMIT;
