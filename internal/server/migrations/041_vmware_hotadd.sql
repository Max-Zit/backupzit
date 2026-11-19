-- Hot-add: the proxy agent, a Linux VM that sees the datastores, reads the
-- disks of running VMs attached to itself instead of copies on the
-- datastore. Off by default: existing hosts keep copying.
ALTER TABLE vmware_hosts ADD COLUMN hot_add BOOLEAN NOT NULL DEFAULT false;
