-- Agents started from recovery media (WinPE) to restore a machine.
ALTER TABLE agents ADD COLUMN recovery BOOLEAN NOT NULL DEFAULT false;
