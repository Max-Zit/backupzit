-- Image restore onto different hardware: prepare the restored Windows
-- (boot drivers, extra drivers, boot files).
ALTER TABLE runs ADD COLUMN new_hardware BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE runs ADD COLUMN driver_path TEXT NOT NULL DEFAULT '';
