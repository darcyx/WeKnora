DROP INDEX IF EXISTS idx_channel_lookup;
CREATE UNIQUE INDEX idx_channel_lookup ON im_channel_sessions (platform, user_id, chat_id, tenant_id, agent_id, (COALESCE(im_channel_id, ''))) WHERE deleted_at IS NULL AND thread_id = '';
DROP INDEX IF EXISTS idx_channel_thread_lookup;
CREATE UNIQUE INDEX idx_channel_thread_lookup ON im_channel_sessions (platform, chat_id, thread_id, tenant_id, agent_id, (COALESCE(im_channel_id, ''))) WHERE deleted_at IS NULL AND thread_id != '';

-- Bind only legacy sessions with one authoritative, nonempty channel mapping.
UPDATE sessions SET
 user_id = (SELECT 'im_user:' || CAST(m.tenant_id AS TEXT) || ':' || m.im_channel_id || ':' || m.platform || ':' || m.user_id
            FROM im_channel_sessions m JOIN im_channels c ON c.id = m.im_channel_id AND c.tenant_id = m.tenant_id AND c.platform = m.platform AND c.agent_id = m.agent_id
            WHERE m.session_id = sessions.id AND m.tenant_id = sessions.tenant_id AND m.thread_id = '' AND m.deleted_at IS NULL AND m.im_channel_id <> '' AND m.user_id <> ''),
 owner_auth_source = 'im'
WHERE (sessions.user_id IS NULL OR sessions.user_id = '') AND sessions.deleted_at IS NULL AND sessions.kind = ''
 AND (SELECT COUNT(*) FROM im_channel_sessions all_m WHERE all_m.session_id = sessions.id AND all_m.tenant_id = sessions.tenant_id AND all_m.deleted_at IS NULL) = 1
 AND (SELECT COUNT(*) FROM im_channel_sessions m JOIN im_channels c ON c.id = m.im_channel_id AND c.tenant_id = m.tenant_id AND c.platform = m.platform AND c.agent_id = m.agent_id WHERE m.session_id = sessions.id AND m.tenant_id = sessions.tenant_id AND m.thread_id = '' AND m.deleted_at IS NULL AND m.im_channel_id <> '' AND m.user_id <> '') = 1;
