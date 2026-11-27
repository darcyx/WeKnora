-- Explicit channel IDs remain authoritative even if a channel later changes agent.
UPDATE sessions SET
 user_id = (SELECT 'im_user:' || CAST(m.tenant_id AS TEXT) || ':' || m.im_channel_id || ':' || m.platform || ':' || m.user_id
            FROM im_channel_sessions m JOIN im_channels c ON c.id = m.im_channel_id AND c.tenant_id = m.tenant_id AND c.platform = m.platform
            WHERE m.session_id = sessions.id AND m.tenant_id = sessions.tenant_id AND m.thread_id = '' AND m.deleted_at IS NULL AND m.im_channel_id <> '' AND m.user_id <> ''),
 owner_auth_source = 'im'
WHERE (sessions.user_id IS NULL OR sessions.user_id = '') AND sessions.deleted_at IS NULL AND sessions.kind = ''
 AND (SELECT COUNT(*) FROM im_channel_sessions m WHERE m.session_id = sessions.id AND m.tenant_id = sessions.tenant_id AND m.deleted_at IS NULL) = 1
 AND (SELECT COUNT(*) FROM im_channel_sessions m JOIN im_channels c ON c.id = m.im_channel_id AND c.tenant_id = m.tenant_id AND c.platform = m.platform WHERE m.session_id = sessions.id AND m.tenant_id = sessions.tenant_id AND m.thread_id = '' AND m.deleted_at IS NULL AND m.im_channel_id <> '' AND m.user_id <> '') = 1;
