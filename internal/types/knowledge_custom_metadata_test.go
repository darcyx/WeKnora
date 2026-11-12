package types

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestKnowledgeCustomMetadataTextIsStableAndSeparate(t *testing.T) {
	knowledge := &Knowledge{
		Metadata:       JSON(`{"external_id":"secret-internal-id"}`),
		CustomMetadata: JSON(`{"region":"Shanghai","department":"R&D"}`),
	}
	require.Equal(t, "department: R&D\nregion: Shanghai", knowledge.CustomMetadataText())
	require.NotContains(t, knowledge.CustomMetadataText(), "external_id")
}

func TestKnowledgeCustomMetadataExcludesImportedProcessingConfiguration(t *testing.T) {
	knowledge := &Knowledge{CustomMetadata: JSON(`{"region":"CN","process_overrides":{"parser_engine":"internal","asr_config":{"api_key":"test-only"}}}`)}
	require.Equal(t, "region: CN", knowledge.CustomMetadataText())
	require.Contains(t, string(knowledge.CustomMetadata), "process_overrides", "rendering must not mutate stored configuration")
}
