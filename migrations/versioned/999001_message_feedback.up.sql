-- Fork-local migration (not upstreamed): versioned 999001+ instead of the next
-- sequential 0000NN so it never collides with upstream's own numbering when
-- this fork merges from Tencent/WeKnora. golang-migrate only requires unique,
-- increasing version numbers, not contiguous ones. Next local migration: 999002.
ALTER TABLE messages ADD COLUMN IF NOT EXISTS feedback JSONB;
