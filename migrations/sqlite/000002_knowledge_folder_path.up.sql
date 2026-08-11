-- Folder tree support for knowledge bases (SQLite / lite mode).
--
-- Mirrors migrations/versioned/000081_knowledge_folder_path.up.sql.
ALTER TABLE knowledges ADD COLUMN folder_path VARCHAR(1024) NOT NULL DEFAULT '';

-- Backfill legacy folder uploads, which encoded their relative directory in
-- file_name. SQLite has no regex, so the directory prefix is isolated with the
-- rtrim trick: trimming every non-slash character off the right leaves exactly
-- the prefix up to and including the last '/'.
UPDATE knowledges
SET folder_path = substr(
        rtrim(file_name, replace(file_name, '/', '')),
        1,
        length(rtrim(file_name, replace(file_name, '/', ''))) - 1
    ),
    file_name = substr(
        file_name,
        length(rtrim(file_name, replace(file_name, '/', ''))) + 1
    )
WHERE file_name LIKE '%/%';

CREATE INDEX IF NOT EXISTS idx_knowledges_folder_path
    ON knowledges (tenant_id, knowledge_base_id, folder_path);


-- Token quota tables for existing SQLite deployments. New databases receive
-- the same schema from 000000_init; this ordered migration upgrades databases
-- whose SQLite migration history is already at version 0 or 1.
CREATE TABLE IF NOT EXISTS token_quota_overrides (
                                                     subject_id VARCHAR(160) PRIMARY KEY,
    daily_token_limit BIGINT NULL,
    monthly_token_limit BIGINT NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
    );

CREATE TABLE IF NOT EXISTS token_quota_period_usages (
                                                         subject_id VARCHAR(160) NOT NULL,
    period VARCHAR(8) NOT NULL,
    period_start DATE NOT NULL,
    prompt_tokens BIGINT NOT NULL DEFAULT 0,
    completion_tokens BIGINT NOT NULL DEFAULT 0,
    total_tokens BIGINT NOT NULL DEFAULT 0,
    reserved_tokens BIGINT NOT NULL DEFAULT 0,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (subject_id, period, period_start)
    );

CREATE TABLE IF NOT EXISTS token_quota_reservations (
                                                        id VARCHAR(36) PRIMARY KEY,
    subject_id VARCHAR(160) NOT NULL,
    day_start DATE NOT NULL,
    month_start DATE NOT NULL,
    reserved_tokens BIGINT NOT NULL,
    status VARCHAR(16) NOT NULL,
    expires_at DATETIME NOT NULL,
    settled_at DATETIME NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
    );

CREATE INDEX IF NOT EXISTS idx_token_quota_reservations_subject_status_expiry
    ON token_quota_reservations (subject_id, status, expires_at);
