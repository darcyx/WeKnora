package postgres

import (
	"context"
	"os"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
	gormpostgres "gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// Run against an empty, disposable database named weknora_keyword_test with pg_search installed.
func TestKeywordRetrievalParadeDB(t *testing.T) {
	dsn := os.Getenv("WEKNORA_PARADEDB_TEST_DSN")
	if dsn == "" {
		t.Skip("set WEKNORA_PARADEDB_TEST_DSN to a disposable ParadeDB database")
	}
	db, err := gorm.Open(gormpostgres.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = sqlDB.Close() })
	var database string
	require.NoError(t, db.Raw("SELECT current_database()").Scan(&database).Error)
	require.Equal(t, "weknora_keyword_test", database)
	var existing *string
	require.NoError(t, db.Raw("SELECT to_regclass('public.embeddings')::text").Scan(&existing).Error)
	require.Nil(t, existing, "use an empty disposable database")
	require.NoError(t, db.Exec(`CREATE TABLE embeddings (
		id serial primary key, source_id text NOT NULL, source_type integer NOT NULL,
		chunk_id text, knowledge_id text, knowledge_base_id text, tag_id text,
		content text, is_enabled boolean
	)`).Error)
	t.Cleanup(func() { _ = db.Exec("DROP TABLE IF EXISTS embeddings").Error })
	require.NoError(t, db.Exec(`CREATE INDEX embeddings_search_idx ON embeddings
		USING bm25 (id, knowledge_base_id, content, knowledge_id, chunk_id)
		WITH (key_field='id', text_fields='{"content":{"tokenizer":{"type":"chinese_lindera"}}}')`).Error)
	require.NoError(t, db.Exec(`INSERT INTO embeddings (source_id,source_type,chunk_id,knowledge_id,knowledge_base_id,tag_id,content,is_enabled) VALUES
		('chunk-1',0,'chunk-1','doc-1','kb-1','tag-1','支付发货地址配置',true),
		('chunk-2',0,'chunk-2','doc-2','kb-1','tag-1','支付发货地址',false),
		('chunk-3',0,'chunk-3','doc-3','kb-2','tag-1','支付发货地址',true),
		('chunk-1-q',0,'chunk-1','doc-1','kb-1','tag-1','支付发货地址',true)`).Error)
	repo := NewPostgresRetrieveEngineRepository(db).(*pgRepository)
	results, err := repo.KeywordsRetrieve(context.Background(), types.RetrieveParams{
		Query: "发货地址", TopK: 150, KnowledgeBaseIDs: []string{"kb-1"},
		ExcludeGeneratedQuestionKBIDs: []string{"kb-1"},
	})
	require.NoError(t, err)
	require.Len(t, results, 1)
	require.Len(t, results[0].Results, 1)
	require.Equal(t, "chunk-1", results[0].Results[0].ChunkID)
	require.Greater(t, results[0].Results[0].Score, float64(0))

	// Exercise the fallback directly with all supported scope filters. Keep
	// generated questions for kb-2 and ensure an unrelated tag returns no rows.
	params := types.RetrieveParams{Query: "发货地址", TopK: 1, KnowledgeBaseIDs: []string{"kb-1", "kb-2"}, KnowledgeIDs: []string{"doc-3"}, TagIDs: []string{"tag-1"}}
	rows, err := repo.keywordMaterializedFallback(context.Background(), params, "paradedb.score")
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "chunk-3", rows[0].ChunkID)
	params.TagIDs = []string{"not-the-tag"}
	rows, err = repo.keywordMaterializedFallback(context.Background(), params, "paradedb.score")
	require.NoError(t, err)
	require.Empty(t, rows)
}
