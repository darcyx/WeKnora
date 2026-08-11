-- Fold the relative directory back into file_name before dropping the column.
UPDATE knowledges
SET file_name = folder_path || '/' || file_name
WHERE folder_path <> '' AND file_name <> '';

DROP INDEX IF EXISTS idx_knowledges_folder_path;

ALTER TABLE knowledges DROP COLUMN folder_path;


DROP INDEX IF EXISTS idx_token_quota_reservations_subject_status_expiry;
DROP TABLE IF EXISTS token_quota_reservations;
DROP TABLE IF EXISTS token_quota_period_usages;
DROP TABLE IF EXISTS token_quota_overrides;
