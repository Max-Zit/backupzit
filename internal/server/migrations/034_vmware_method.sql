-- How a VMware host is changed: '' automatic (API on licensed hosts, SSH on
-- the free license), 'ssh' or 'api'.
ALTER TABLE vmware_hosts ADD COLUMN method TEXT NOT NULL DEFAULT '';
