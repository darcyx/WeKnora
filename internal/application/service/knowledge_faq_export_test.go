package service

import (
	"encoding/csv"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func TestBuildFAQCSVIncludesEntryID(t *testing.T) {
	metadata := &types.FAQChunkMetadata{
		StandardQuestion: "如何重置密码？",
		Answers:          []string{"打开设置页面", "选择重置密码"},
	}
	chunk := &types.Chunk{SeqID: 100000123, Metadata: mustFAQMetadata(t, metadata)}

	data := (&knowledgeService{}).buildFAQCSV([]*types.Chunk{chunk}, nil)
	records, err := csv.NewReader(strings.NewReader(string(data))).ReadAll()
	require.NoError(t, err)
	require.Len(t, records, 2)
	require.Equal(t, "ID", records[0][0])
	require.Equal(t, "100000123", records[1][0])
	require.Equal(t, "如何重置密码？", records[1][2])
}

func mustFAQMetadata(t *testing.T, metadata *types.FAQChunkMetadata) types.JSON {
	t.Helper()

	chunk := &types.Chunk{}
	require.NoError(t, chunk.SetFAQMetadata(metadata))
	return chunk.Metadata
}
