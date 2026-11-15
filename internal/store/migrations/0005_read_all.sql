-- read_all (issue #38) splits reading a queue's transcripts, results,
-- prompts and metadata off view_all, and queue_admin no longer implies it.
-- So that existing deployments keep exactly the access they had, every
-- view_all and queue_admin grant on a queue gains a read_all grant on the
-- same queue, attributed to the original granter. Workers never held
-- either role, but the principal filter makes that explicit.
INSERT OR IGNORE INTO grants (principal, role, scope, granted_by, granted_at)
  SELECT principal, 'read_all', scope, granted_by, granted_at FROM grants
  WHERE role IN ('view_all', 'queue_admin')
    AND scope LIKE 'queue:%'
    AND (principal LIKE 'user:%' OR principal LIKE 'client:%');
