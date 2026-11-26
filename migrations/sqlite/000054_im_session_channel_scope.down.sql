-- Abort before changing indexes if channels share a former session key.
CREATE UNIQUE INDEX idx_im_rollback_check ON im_channel_sessions
    (platform, chat_id, tenant_id, agent_id, (CASE WHEN thread_id = '' THEN 'u:' || user_id ELSE 't:' || thread_id END))
    WHERE deleted_at IS NULL;
DROP INDEX IF EXISTS idx_channel_lookup;
CREATE UNIQUE INDEX idx_channel_lookup ON im_channel_sessions (platform, user_id, chat_id, tenant_id, agent_id) WHERE deleted_at IS NULL AND thread_id = '';
DROP INDEX IF EXISTS idx_channel_thread_lookup;
CREATE UNIQUE INDEX idx_channel_thread_lookup ON im_channel_sessions (platform, chat_id, thread_id, tenant_id, agent_id) WHERE deleted_at IS NULL AND thread_id != '';
DROP INDEX idx_im_rollback_check;
