-- Proxmox VE: guest inventory reported by agents on Proxmox nodes, and the
-- options of VM restore runs.
ALTER TABLE agents ADD COLUMN hypervisor JSONB;
ALTER TABLE agents ADD COLUMN hypervisor_at TIMESTAMPTZ;
ALTER TABLE runs ADD COLUMN vm_restore JSONB;
