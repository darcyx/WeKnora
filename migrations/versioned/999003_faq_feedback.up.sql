CREATE TABLE IF NOT EXISTS faq_feedbacks (
    tenant_id BIGINT NOT NULL,
    session_id TEXT NOT NULL,
    user_id TEXT NOT NULL,
    entry_id BIGINT NOT NULL,
    feedback JSONB NOT NULL,
    chunk_id TEXT,
    knowledge_id TEXT,
    knowledge_base_id TEXT,
    tag_id BIGINT,
    tag_name TEXT,
    standard_question TEXT,
    similar_questions JSONB,
    negative_questions JSONB,
    answers JSONB,
    answer_strategy TEXT,
    PRIMARY KEY (tenant_id, session_id, entry_id)
);
