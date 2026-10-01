-- S3 compatible storage targets. url is s3://endpoint/bucket/prefix[?tls=false].
ALTER TABLE storage_targets ADD COLUMN s3_access_key TEXT NOT NULL DEFAULT '';
ALTER TABLE storage_targets ADD COLUMN s3_secret_key TEXT NOT NULL DEFAULT '';
ALTER TABLE storage_targets ADD COLUMN s3_region TEXT NOT NULL DEFAULT '';
