package repository

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestFindExactFAQQuestionsScope(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&types.Chunk{}))
	require.NoError(t, db.Exec("CREATE TABLE knowledges (id TEXT, tenant_id INTEGER, knowledge_base_id TEXT, deleted_at DATETIME, enable_status TEXT)").Error)
	require.NoError(t, db.Exec("INSERT INTO knowledges (id, tenant_id, knowledge_base_id, enable_status) VALUES ('doc', 1, 'kb', 'enabled'), ('doc', 2, 'kb', 'enabled')").Error)
	for _, c := range []types.Chunk{
		{ID: "match", TenantID: 1, KnowledgeBaseID: "kb", KnowledgeID: "doc", TagID: "tag", ChunkType: types.ChunkTypeFAQ, IsEnabled: true, Metadata: types.JSON(`{"standard_question":"A","similar_questions":["Q"]}`)},
		{ID: "wrong-tenant", TenantID: 2, KnowledgeBaseID: "kb", KnowledgeID: "doc", TagID: "tag", ChunkType: types.ChunkTypeFAQ, IsEnabled: true, Metadata: types.JSON(`{"standard_question":"Q"}`)},
		{ID: "wrong-tag", TenantID: 1, KnowledgeBaseID: "kb", KnowledgeID: "doc", TagID: "other", ChunkType: types.ChunkTypeFAQ, IsEnabled: true, Metadata: types.JSON(`{"standard_question":"Q"}`)},
		{ID: "disabled", TenantID: 1, KnowledgeBaseID: "kb", KnowledgeID: "doc", TagID: "tag", ChunkType: types.ChunkTypeFAQ, IsEnabled: false, Metadata: types.JSON(`{"standard_question":"Q"}`)},
	} {
		// GORM's default:true otherwise replaces false on insert.
		require.NoError(t, db.Create(&c).Error)
		if c.ID == "disabled" {
			require.NoError(t, db.Model(&c).Update("is_enabled", false).Error)
		}
	}
	repo := NewChunkRepository(db)
	target := &types.SearchTarget{TenantID: 1, KnowledgeBaseID: "kb", Type: types.SearchTargetTypeKnowledge, KnowledgeIDs: []string{"doc"}, TagIDs: []string{"tag"}}
	rows, err := repo.FindExactFAQQuestions(context.Background(), target, "Q")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "match", rows[0].ID)
	require.NoError(t, db.Exec("UPDATE knowledges SET enable_status = 'disabled' WHERE tenant_id = 1").Error)
	rows, err = repo.FindExactFAQQuestions(context.Background(), target, "Q")
	require.NoError(t, err)
	require.Empty(t, rows)
	require.NoError(t, db.Exec("UPDATE knowledges SET enable_status = 'enabled' WHERE tenant_id = 1").Error)
	require.NoError(t, db.Exec("UPDATE knowledges SET deleted_at = CURRENT_TIMESTAMP WHERE tenant_id = 1").Error)
	rows, err = repo.FindExactFAQQuestions(context.Background(), target, "Q")
	require.NoError(t, err)
	require.Empty(t, rows)
	require.NoError(t, db.Exec("UPDATE knowledges SET deleted_at = NULL WHERE tenant_id = 1").Error)
	target.KnowledgeIDs = []string{"other"}
	rows, err = repo.FindExactFAQQuestions(context.Background(), target, "Q")
	require.NoError(t, err)
	require.Empty(t, rows)
}
