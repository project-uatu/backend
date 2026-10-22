-- Records when each known repository was last fully covered by a commit scan, so
-- repositories discovered after an initial scan are backfilled before the
-- incremental overlap window applies.
ALTER TABLE known_repositories ADD COLUMN commit_scanned_through TIMESTAMPTZ;
