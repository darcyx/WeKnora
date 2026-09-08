CREATE TABLE IF NOT EXISTS faq_feedbacks (
    tenant_id INTEGER NOT NULL,
    session_id TEXT NOT NULL,
    user_id TEXT NOT NULL,
    entry_id INTEGER NOT NULL,
    feedback TEXT NOT NULL,
    chunk_id TEXT,
    knowledge_id TEXT,
    knowledge_base_id TEXT,
    tag_id INTEGER,
    tag_name TEXT,
    standard_question TEXT,
    similar_questions TEXT,
    negative_questions TEXT,
    answers TEXT,
    answer_strategy TEXT,
    PRIMARY KEY (tenant_id, session_id, entry_id)
);
