-- The source address key (auth.IPKey) a worker registered from, so the
-- number of pending workers per source can be capped. Empty for workers
-- registered before this column existed.
ALTER TABLE workers ADD COLUMN registered_from TEXT NOT NULL DEFAULT '';
CREATE INDEX workers_pending_source ON workers (registered_from) WHERE state = 'pending';
