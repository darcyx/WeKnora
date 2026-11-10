-- Fork-local migration (not upstreamed): versioned 999002+ instead of the next
-- sequential 0000NN so it never collides with upstream's own numbering when
-- this fork merges from Tencent/WeKnora. golang-migrate only requires unique,
-- increasing version numbers, not contiguous ones. Next local migration: 999003.
--
-- Platform-wide token quota for API integration external users. The subject is
-- deliberately not a foreign key: it comes from the trusted external-user
-- principal and can exist without a local WeKnora account.
CREATE TABLE IF NOT EXISTS token_quota_overrides (
    subject_id VARCHAR(160) PRIMARY KEY,
    daily_token_limit BIGINT NULL,
    monthly_token_limit BIGINT NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS token_quota_period_usages (
    subject_id VARCHAR(160) NOT NULL,
    period VARCHAR(8) NOT NULL,
    period_start DATE NOT NULL,
    prompt_tokens BIGINT NOT NULL DEFAULT 0,
    completion_tokens BIGINT NOT NULL DEFAULT 0,
    total_tokens BIGINT NOT NULL DEFAULT 0,
    reserved_tokens BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    PRIMARY KEY (subject_id, period, period_start)
);

CREATE TABLE IF NOT EXISTS token_quota_reservations (
    id VARCHAR(36) PRIMARY KEY,
    subject_id VARCHAR(160) NOT NULL,
    day_start DATE NOT NULL,
    month_start DATE NOT NULL,
    reserved_tokens BIGINT NOT NULL,
    status VARCHAR(16) NOT NULL,
    expires_at TIMESTAMP WITH TIME ZONE NOT NULL,
    settled_at TIMESTAMP WITH TIME ZONE NULL,
    created_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMP WITH TIME ZONE NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_token_quota_reservations_subject_status_expiry
    ON token_quota_reservations (subject_id, status, expires_at);
