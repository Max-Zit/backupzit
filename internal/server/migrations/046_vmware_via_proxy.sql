-- A VMware connection reached only through its proxy agent (0.56, #51):
-- the proxy checks the credentials and reads the VM list (vmware-probe
-- runs), so the console needs no route to the host.
ALTER TABLE vmware_hosts ADD COLUMN via_proxy BOOLEAN NOT NULL DEFAULT false;
