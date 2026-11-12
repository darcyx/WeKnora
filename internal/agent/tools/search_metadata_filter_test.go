package tools

import (
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func TestMetadataMatches(t *testing.T) {
	t.Parallel()
	custom := types.JSON(`{"region":"Shanghai","version":2,"products":["Echo","Atlas"]}`)
	cases := []struct {
		name string
		want map[string]string
		ok   bool
	}{
		{"case-insensitive string", map[string]string{"region": " shanghai "}, true},
		{"number value", map[string]string{"version": "2"}, true},
		{"list element", map[string]string{"products": "atlas"}, true},
		{"all keys required", map[string]string{"region": "Shanghai", "version": "3"}, false},
		{"missing key", map[string]string{"owner": "Alice"}, false},
	}
	for _, tc := range cases {
		require.Equal(t, tc.ok, metadataMatches(custom, tc.want), tc.name)
	}
	require.False(t, metadataMatches(types.JSON(`{}`), map[string]string{"region": "Shanghai"}))
	require.False(t, metadataMatches(nil, map[string]string{"region": "Shanghai"}))
}

func TestRestrictTargetKeepsScopeWithoutMutating(t *testing.T) {
	t.Parallel()
	whole := &types.SearchTarget{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "kb", TagIDs: []string{"t"}}
	got := restrictTarget(whole, []string{"d1", "d2"})
	require.Equal(t, types.SearchTargetTypeKnowledge, got.Type)
	require.Equal(t, []string{"d1", "d2"}, got.KnowledgeIDs)
	require.Equal(t, []string{"t"}, got.TagIDs)
	require.Equal(t, types.SearchTargetTypeKnowledgeBase, whole.Type)
	require.Empty(t, whole.KnowledgeIDs)

	pinned := &types.SearchTarget{Type: types.SearchTargetTypeKnowledge, KnowledgeBaseID: "kb", KnowledgeIDs: []string{"d2", "d3"}}
	require.Equal(t, []string{"d2"}, restrictTarget(pinned, []string{"d1", "d2"}).KnowledgeIDs)
	require.Nil(t, restrictTarget(pinned, []string{"d1"}))
	require.Nil(t, restrictTarget(whole, nil))
}
