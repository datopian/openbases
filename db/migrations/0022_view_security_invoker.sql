-- Make credential_rotation_due respect the policy on the table beneath it.
--
-- SECURITY FIX. The view was a complete bypass of the admin-only read policy on
-- credential_registry, and a test proved it before this was written: a project
-- lead with no administrative role could read every row through the view.
--
-- Since PostgreSQL 15 a view is evaluated with the permissions of its OWNER
-- unless it is declared security_invoker. These migrations are applied by a
-- superuser, so the owner is a superuser, and a superuser does not merely satisfy
-- row-level security — it never evaluates it. The policy on credential_registry
-- was therefore never consulted for anybody reading the view, while the table
-- itself was correctly protected. The protection and the hole sat four lines
-- apart in the same migration.
--
-- What was exposed is worth being precise about, because it is not secret values:
-- the registry holds references. Names, owners, which store each credential lives
-- in, how it is delivered to a process, its blast radius and who can revoke it.
-- That is a map of every credential in the company and where to go looking for
-- it, which is the first thing worth having if you are trying to escalate.
--
-- security_invoker = true evaluates the view as the CALLER, so the policy applies
-- to whoever is asking. An organisation admin still sees everything; nobody else
-- sees anything.

BEGIN;

ALTER VIEW credential_rotation_due SET (security_invoker = true);

COMMIT;
