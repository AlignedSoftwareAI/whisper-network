-- When a lease was last heartbeated, for the UI (DESIGN.md §6.3). NULL until
-- the first heartbeat. expires_at minus the TTL is not a substitute: the TTL
-- is server configuration that can change across restarts, and the startup
-- extension (§8.3) moves expires_at without any heartbeat.
ALTER TABLE leases ADD COLUMN heartbeat_at INTEGER;
