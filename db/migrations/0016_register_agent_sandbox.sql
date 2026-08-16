-- Register the agent sandbox repository.
--
-- datopian/workgraph-agent-sandbox is where the pilot agent does its work: it
-- holds the rig Gas Town clones, and it is where the first agent-authored pull
-- request was opened. It was created during WP-E3 and never added to the
-- registry, which nothing depended on until now.
--
-- The deterministic witness (ADR-0019) escalates through the registry: a rig's
-- git URL maps to a repository, a repository maps to a project, and a project
-- has the lead who should hear that an agent left uncommitted work behind. With
-- the repository absent, every escalation from the sandbox rig resolved to no
-- project and reached nobody. That is exactly what the first live pass reported
-- — seven escalations, zero notified — which is the failure this migration
-- fixes rather than a hypothetical one.
--
-- It belongs to portaljs-oss for the same reason autoclaw.sh does: the sandbox
-- is open-source pilot tooling, not client work, and it must not sit in a
-- project whose visibility is restricted.

BEGIN;

INSERT INTO project_repositories (project_id, owner, name)
SELECT p.id, 'datopian', 'workgraph-agent-sandbox'
FROM projects p
WHERE p.slug = 'portaljs-oss'
ON CONFLICT (provider, owner, name) DO NOTHING;

COMMIT;
