-- The credential registry (plan §11.4, WP-B3).
--
-- References, owners and expiry — never values. A credential value in the
-- database would defeat the entire point of encrypting it at rest elsewhere,
-- and the database is the one place in this system that gets backed up,
-- replicated and read by an application that serves untrusted input.
--
-- What this is FOR is the question rotation cannot answer on its own: which
-- credentials exist, who is accountable for each, where it actually lives, and
-- when it was last rotated. Without it, "rotate everything" means reading a
-- shell script and hoping it is complete, and "is anything overdue" has no
-- answer at all.

BEGIN;

CREATE TABLE credential_registry (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    -- The name used everywhere else: the key in infra/secrets/*.enc.yaml, and
    -- the systemd credential name where it is one. One name, so a rotation
    -- script and a human are talking about the same thing.
    name          text NOT NULL UNIQUE,
    description   text NOT NULL,

    -- Where the authoritative copy lives. Not a value, a location.
    store         text NOT NULL CHECK (store IN ('sops', 'cloudflare', 'github', 'hetzner', 'postgres', 'external')),
    -- How the running process receives it, which is what an incident responder
    -- needs to know to find it on a host.
    delivery      text NOT NULL CHECK (delivery IN ('systemd-credential', 'environment', 'file', 'api-only')),

    -- Accountability. A credential with no owner is a credential nobody
    -- rotates, and every one of these can be revoked by a person who has to be
    -- identifiable.
    owner_email   text NOT NULL,

    -- Blast radius, recorded deliberately rather than inferred at 3am.
    scope         text NOT NULL,
    revocable_by  text NOT NULL,

    rotated_at    timestamptz,
    -- NULL means it does not expire on its own, which is true of most of these
    -- and is itself worth knowing: a credential that never expires is one that
    -- only rotation will ever change.
    expires_at    timestamptz,
    -- How often it SHOULD be rotated, whether or not the provider forces it.
    rotate_every  interval,

    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()

    -- Deliberately NO constraint tying rotate_every to rotated_at.
    --
    -- The first version required a rotation date whenever an interval was set,
    -- on the reasoning that an interval with no baseline cannot produce a due
    -- date. That rejected the seed, and the constraint was the thing that was
    -- wrong: "should be rotated every 90 days, never has been" is not an
    -- inconsistency, it is the single most important row in the table. The
    -- credential_rotation_due view reports it as 'never rotated', which is
    -- exactly the signal wanted.
);

-- Values must never land here. This is belt and braces over review: a column
-- added later called `secret` or `token` would otherwise be the obvious place
-- for someone to put one.
CREATE OR REPLACE FUNCTION credential_registry_holds_no_values() RETURNS trigger AS $$
BEGIN
    IF NEW.description ~* '(-----BEGIN|cfat_|gh[pousr]_|GOCSPX-|AKIA[0-9A-Z]{16})' THEN
        RAISE EXCEPTION 'credential_registry records references, not values';
    END IF;
    NEW.updated_at := now();
    RETURN NEW;
END
$$ LANGUAGE plpgsql;

CREATE TRIGGER credential_registry_guard
    BEFORE INSERT OR UPDATE ON credential_registry
    FOR EACH ROW EXECUTE FUNCTION credential_registry_holds_no_values();

ALTER TABLE credential_registry ENABLE ROW LEVEL SECURITY;
ALTER TABLE credential_registry FORCE ROW LEVEL SECURITY;

-- Readable by organisation admins only. It names every credential in the system
-- and who can revoke it, which is a map of the blast radius — useful to an
-- operator and equally useful to an attacker who has a session.
CREATE POLICY credential_registry_admin_read ON credential_registry
    FOR SELECT
    USING (
        EXISTS (
            SELECT 1 FROM role_grants g
             WHERE g.user_id = current_app_user()
               AND g.role_name = 'organisation_admin'
               AND g.project_id IS NULL
        )
    );

-- What is overdue. A view rather than a query someone has to remember, because
-- the question gets asked during an incident and not before one.
CREATE VIEW credential_rotation_due AS
SELECT name, description, owner_email, store, delivery, scope, revocable_by,
       rotated_at, expires_at, rotate_every,
       CASE
           WHEN expires_at IS NOT NULL AND expires_at <= now() THEN 'expired'
           WHEN rotated_at IS NULL THEN 'never rotated'
           WHEN rotate_every IS NOT NULL AND rotated_at + rotate_every <= now() THEN 'overdue'
           ELSE 'ok'
       END AS state,
       CASE
           WHEN rotate_every IS NOT NULL AND rotated_at IS NOT NULL
           THEN rotated_at + rotate_every
       END AS due_at
  FROM credential_registry;

CREATE OR REPLACE FUNCTION system_record_rotation(p_name text, p_when timestamptz)
RETURNS boolean
LANGUAGE plpgsql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
DECLARE updated integer;
BEGIN
    UPDATE credential_registry
       SET rotated_at = COALESCE(p_when, now())
     WHERE name = p_name;
    GET DIAGNOSTICS updated = ROW_COUNT;
    -- false means the credential is not registered. The rotation script treats
    -- that as a failure: a credential nobody registered is a credential nobody
    -- is accountable for, and silently doing nothing would hide that.
    RETURN updated > 0;
END
$$;

CREATE OR REPLACE FUNCTION system_credential_rotation_due()
RETURNS TABLE (name text, owner_email text, state text, due_at timestamptz)
LANGUAGE sql
SECURITY DEFINER
SET search_path = public, pg_temp
AS $$
    SELECT v.name, v.owner_email, v.state, v.due_at
      FROM credential_rotation_due v
     WHERE v.state <> 'ok'
     ORDER BY v.due_at NULLS FIRST, v.name;
$$;

REVOKE ALL ON FUNCTION system_record_rotation(text, timestamptz) FROM PUBLIC;
REVOKE ALL ON FUNCTION system_credential_rotation_due() FROM PUBLIC;
GRANT EXECUTE ON FUNCTION system_record_rotation(text, timestamptz) TO workgraph_app;
GRANT EXECUTE ON FUNCTION system_credential_rotation_due() TO workgraph_app;
GRANT SELECT ON credential_rotation_due TO workgraph_app;

COMMIT;
