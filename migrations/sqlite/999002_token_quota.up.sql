-- Mirrors versioned migration 999002_token_quota (fork-local, not
-- upstreamed — see the comment there for why it's numbered 999002 instead of
-- the next sequential 0000NN).
-- Platform-wide token quota for API integration external users.

CREATE TABLE IF NOT EXISTS token_quota_overrides (
    subject_id VARCHAR(160) PRIMARY KEY,
    daily_token_limit INTEGER NULL,
    monthly_token_limit INTEGER NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS token_quota_period_usages (
    subject_id VARCHAR(160) NOT NULL,
    period VARCHAR(8) NOT NULL,
    period_start DATE NOT NULL,
    prompt_tokens INTEGER NOT NULL DEFAULT 0,
    completion_tokens INTEGER NOT NULL DEFAULT 0,
    total_tokens INTEGER NOT NULL DEFAULT 0,
    reserved_tokens INTEGER NOT NULL DEFAULT 0,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (subject_id, period, period_start)
);

CREATE TABLE IF NOT EXISTS token_quota_reservations (
    id VARCHAR(36) PRIMARY KEY,
    subject_id VARCHAR(160) NOT NULL,
    day_start DATE NOT NULL,
    month_start DATE NOT NULL,
    reserved_tokens INTEGER NOT NULL,
    status VARCHAR(16) NOT NULL,
    expires_at DATETIME NOT NULL,
    settled_at DATETIME NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_token_quota_reservations_subject_status_expiry
    ON token_quota_reservations (subject_id, status, expires_at);
