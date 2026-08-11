-- Mirrors versioned migration 999001_message_feedback (fork-local, not
-- upstreamed — see the comment there for why it's numbered 999001 instead of
-- the next sequential 0000NN).
-- Per-message like/dislike vote, with reasons for a dislike.

ALTER TABLE messages ADD COLUMN feedback TEXT;
