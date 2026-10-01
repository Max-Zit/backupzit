-- Ransomware / mass deletion detection.
ALTER TABLE runs ADD COLUMN anomaly TEXT NOT NULL DEFAULT '';
ALTER TABLE runs ADD COLUMN anomaly_ack BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE jobs ADD COLUMN retention_hold BOOLEAN NOT NULL DEFAULT false;
