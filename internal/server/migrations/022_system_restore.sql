-- Linux system restore options (target disk or new Proxmox VM).
ALTER TABLE runs ADD COLUMN system_restore JSONB;
