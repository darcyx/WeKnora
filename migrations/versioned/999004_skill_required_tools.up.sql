ALTER TABLE tenant_skills ADD COLUMN IF NOT EXISTS required_tools JSONB;
ALTER TABLE tenant_skill_catalog ADD COLUMN IF NOT EXISTS required_tools JSONB;
