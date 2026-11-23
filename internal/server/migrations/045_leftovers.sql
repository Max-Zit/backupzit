-- What an agent's backups left on a hypervisor and could not remove yet
-- (snapshots, checkpoints, temporary disk copies, backup locks), as the
-- agent reports its ledger (0.56); the dashboard lists them.
ALTER TABLE agents ADD COLUMN leftovers JSONB;
ALTER TABLE agents ADD COLUMN leftovers_at TIMESTAMPTZ;
