-- Agent health: what the witness saw, and how it reaches a person.
--
-- The deterministic witness (internal/witness) replaces Gas Town's patrol
-- agent. It settles the routine cases itself and escalates the ones that could
-- lose work. "Escalate" has to mean something concrete, so it means an item in
-- the attention inbox — which until now nothing wrote to.
--
-- Two things are needed for that to be safe. First, the witness runs on an
-- execution node with no user identity, exactly like the webhook and the
-- reconciliation timer, so it reaches the table through a narrow SECURITY
-- DEFINER function rather than by being handed a role that can read everything
-- (the pattern established in 0013 and 0014). Second, a standing problem is
-- observed on every pass, so the insert has to be idempotent per situation or
-- the inbox fills with copies of one finding and becomes useless.

BEGIN;

-- ---------------------------------------------------------------------------
-- Deduplication
-- ---------------------------------------------------------------------------
--
-- The key identifies the situation — rig, polecat, and what is wrong — not the
-- observation. A polecat that is still dirty on the next pass updates the item
-- it already has; one whose problem changes raises a new one.
--
-- Nullable, because every existing rule computes its items fresh from the graph
-- and has no situation to key on. The unique index is partial on that account,
-- and also on status: once an item is resolved, the same problem recurring is a
-- new event that deserves a new row rather than the silent resurrection of an
-- old one.
ALTER TABLE attention_items ADD COLUMN dedupe_key text;
ALTER TABLE attention_items ADD COLUMN last_seen_at timestamptz;

CREATE UNIQUE INDEX attention_items_open_dedupe
    ON attention_items (user_id, dedupe_key)
    WHERE dedupe_key IS NOT NULL AND status IN ('open', 'snoozed');

-- ---------------------------------------------------------------------------
-- The record of what the witness did
-- ---------------------------------------------------------------------------
--
-- Every decision, not only the escalations. The claim this work rests on is
-- that health monitoring can run without inference, and that claim is only
-- checkable if the observations are counted too: a pass that decided nothing is
-- the evidence, and it is invisible if only exceptions are stored.
CREATE TABLE agent_health_events (
    id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    cell        text NOT NULL,
    rig         text NOT NULL,
    polecat     text NOT NULL,
    action      text NOT NULL CHECK (action IN ('observe', 'nuke', 'escalate', 'ambiguous')),
    reason      text NOT NULL,
    -- The field values the decision was made from, so an action can be
    -- reviewed against what was actually visible at the time rather than
    -- against what the code would do today.
    basis       text NOT NULL,
    bead        text,
    -- Whether settling this case required a model. The whole point of the
    -- exercise is that this is false, and a column makes it a measurement
    -- rather than an assertion.
    used_model  boolean NOT NULL DEFAULT false,
    observed_at timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX agent_health_events_recent
    ON agent_health_events (observed_at DESC);

-- Non-observe decisions are the ones anyone will ever query for by hand.
CREATE INDEX agent_health_events_actionable
    ON agent_health_events (cell, rig, polecat, observed_at DESC)
    WHERE action <> 'observe';

ALTER TABLE agent_health_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE agent_health_events FORCE ROW LEVEL SECURITY;

-- Deliberately no policy. These rows describe infrastructure, not project work,
-- and there is no per-project scoping that would make them safe to expose
-- through the ordinary member path. The system functions below reach them as
-- the owner; every other session sees an empty table.

-- ---------------------------------------------------------------------------
-- The system path
-- ---------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION system_record_agent_health(
    p_cell       text,
    p_rig        text,
    p_polecat    text,
    p_action     text,
    p_reason     text,
    p_basis      text,
    p_bead       text,
    p_used_model boolean
) RETURNS void
LANGUAGE sql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    INSERT INTO agent_health_events (cell, rig, polecat, action, reason, basis, bead, used_model)
    VALUES (p_cell, p_rig, p_polecat, p_action, p_reason, p_basis,
            NULLIF(p_bead, ''), COALESCE(p_used_model, false));
$$;

-- Who an infrastructure problem belongs to.
--
-- A stalled agent in a project's rig is that project's lead's problem. Leads
-- first; if a project has none, its backup operators, who hold takeover rights
-- for exactly this reason; failing both, organisation admins, so that a finding
-- is never silently addressed to nobody.
--
-- Deliberately not "every member": an inbox item is a claim on someone's
-- attention, and sending an infrastructure escalation to every contributor on a
-- project trains people to ignore the inbox.
CREATE OR REPLACE FUNCTION system_attention_recipients(p_project_slug text)
RETURNS TABLE (user_id uuid)
LANGUAGE sql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    WITH proj AS (
        SELECT id FROM projects WHERE slug = p_project_slug
    ),
    ranked AS (
        SELECT m.user_id,
               CASE m.role_name
                   WHEN 'project_lead'    THEN 1
                   WHEN 'backup_operator' THEN 2
               END AS tier
          FROM project_memberships m
          JOIN proj ON proj.id = m.project_id
         WHERE m.role_name IN ('project_lead', 'backup_operator')
        UNION ALL
        -- Organisation admins are the fallback for a project that exists but
        -- has nobody assigned to it. The join to proj is what makes that true:
        -- without it an unknown slug also reaches the admins, and the caller's
        -- "this escalation found nobody" signal — which is how an unregistered
        -- repository gets noticed — never fires.
        SELECT g.user_id, 3
          FROM role_grants g
         CROSS JOIN proj
         WHERE g.role_name = 'organisation_admin'
           AND g.project_id IS NULL
    )
    SELECT DISTINCT user_id FROM ranked
     WHERE tier = (SELECT MIN(tier) FROM ranked);
$$;

-- Which project owns the code a rig is working on.
--
-- The witness knows a rig, and a rig knows its git URL. It does not, and should
-- not, know Workgraph project slugs: configuring the mapping twice is how the
-- two copies come to disagree. The registry already records which project owns
-- which repository, so that is the mapping.
CREATE OR REPLACE FUNCTION system_project_for_repository(p_owner text, p_name text)
RETURNS text
LANGUAGE sql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    SELECT p.slug
      FROM project_repositories r
      JOIN projects p ON p.id = r.project_id
     WHERE r.provider = 'github'
       AND lower(r.owner) = lower(p_owner)
       AND lower(r.name)  = lower(p_name)
     LIMIT 1;
$$;

-- Raise one escalation, idempotently.
--
-- Returns the number of people it reached. Zero is a real answer and the caller
-- is expected to treat it as a failure to escalate rather than a success: a
-- project with no lead, no backup operator and no organisation admin has nobody
-- to tell, and that is worth knowing at the moment it happens rather than when
-- someone eventually notices the agent never came back.
CREATE OR REPLACE FUNCTION system_raise_attention(
    p_project_slug text,
    p_rule         text,
    p_dedupe_key   text,
    p_score        numeric,
    p_explanation  jsonb,
    p_visibility   text
) RETURNS integer
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE
    v_project uuid;
    v_count   integer;
BEGIN
    SELECT id INTO v_project FROM projects WHERE slug = p_project_slug;

    INSERT INTO attention_items
        (user_id, project_id, rule, score, explanation, visibility, dedupe_key, last_seen_at)
    SELECT r.user_id, v_project, p_rule, p_score, p_explanation,
           COALESCE(p_visibility, 'internal'), p_dedupe_key, now()
      FROM system_attention_recipients(p_project_slug) r
    ON CONFLICT (user_id, dedupe_key)
        WHERE dedupe_key IS NOT NULL AND status IN ('open', 'snoozed')
    DO UPDATE SET
        -- The explanation is refreshed because the situation may have moved on
        -- in ways that matter (silent for 20 minutes, then for two hours), but
        -- status is left alone: a snooze the operator set is their decision and
        -- must not be undone by the machine noticing the problem again.
        explanation  = EXCLUDED.explanation,
        score        = EXCLUDED.score,
        last_seen_at = now();

    GET DIAGNOSTICS v_count = ROW_COUNT;
    RETURN v_count;
END
$$;

REVOKE ALL ON FUNCTION system_record_agent_health(text, text, text, text, text, text, text, boolean) FROM PUBLIC;
REVOKE ALL ON FUNCTION system_attention_recipients(text) FROM PUBLIC;
REVOKE ALL ON FUNCTION system_project_for_repository(text, text) FROM PUBLIC;
REVOKE ALL ON FUNCTION system_raise_attention(text, text, text, numeric, jsonb, text) FROM PUBLIC;

GRANT EXECUTE ON FUNCTION system_record_agent_health(text, text, text, text, text, text, text, boolean) TO workgraph_app;
GRANT EXECUTE ON FUNCTION system_attention_recipients(text) TO workgraph_app;
GRANT EXECUTE ON FUNCTION system_project_for_repository(text, text) TO workgraph_app;
GRANT EXECUTE ON FUNCTION system_raise_attention(text, text, text, numeric, jsonb, text) TO workgraph_app;

COMMIT;
