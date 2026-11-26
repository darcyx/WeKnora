package agent

import (
	"encoding/xml"
	"io"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func TestFormatKnowledgeBaseListRendersGeneratedProfile(t *testing.T) {
	kbs := []*KnowledgeBaseInfo{{
		ID: "kb-1", Name: "Ops", Type: "document", Description: "Manual description",
		Profile: &types.KnowledgeBaseProfile{
			Gist:             "Runbooks & guides for the <cluster>",
			Topics:           []string{"Kubernetes", "Ingress"},
			TypicalQuestions: []string{"How do I roll back a deploy?", "q2", "q3", "q4", "q5", "q6"},
		},
	}}
	text := formatKnowledgeBaseList(kbs)
	require.NotContains(t, text, "<description>", "the gist covers the manual description")
	require.NotContains(t, text, "capabilities=", "empty capabilities are omitted")
	require.Contains(t, text, "<generated_profile>")
	require.Contains(t, text, "<gist>Runbooks &amp; guides for the &lt;cluster&gt;</gist>")
	require.Contains(t, text, "<topics>Kubernetes, Ingress</topics>")
	require.Contains(t, text, "<question>How do I roll back a deploy?</question>")
	require.Contains(t, text, "<question>q3</question>")
	require.NotContains(t, text, "<question>q4</question>", "questions are capped")
	decoder := xml.NewDecoder(strings.NewReader(text))
	for {
		_, err := decoder.Token()
		if err == io.EOF {
			break
		}
		require.NoError(t, err, "untrusted profile text must be escaped")
	}
}

func TestFormatKnowledgeBaseListSlimFields(t *testing.T) {
	long := strings.Repeat("长", 500)
	tests := []struct {
		name        string
		kb          *KnowledgeBaseInfo
		contains    []string
		notContains []string
	}{
		{
			name: "description kept and capped without gist",
			kb: &KnowledgeBaseInfo{ID: "kb", Name: "n", Description: long, Capabilities: []string{"chunks"},
				Profile: &types.KnowledgeBaseProfile{Topics: []string{"t"}}},
			contains:    []string{"<description>" + strings.Repeat("长", 160) + "...</description>", `capabilities="chunks"`},
			notContains: []string{strings.Repeat("长", 161)},
		},
		{
			name: "document recent docs drop chunk_id and type",
			kb: &KnowledgeBaseInfo{ID: "kb", Name: "n", Type: "document", RecentDocs: []RecentDocInfo{
				{KnowledgeID: "k1", ChunkID: "c1", Type: "text", Title: long},
			}},
			contains:    []string{`<document knowledge_id="k1"><name>` + strings.Repeat("长", 80) + "...</name>"},
			notContains: []string{"chunk_id=", ` type="text"`},
		},
		{
			name: "faq recent docs keep chunk_id",
			kb: &KnowledgeBaseInfo{ID: "kb", Name: "n", Type: "faq", RecentDocs: []RecentDocInfo{
				{KnowledgeID: "k1", ChunkID: "c1", Type: "faq", FAQStandardQuestion: "Q?"},
			}},
			contains: []string{`<document knowledge_id="k1" chunk_id="c1"><name>Q?</name></document>`},
		},
		{
			name: "profile caps",
			kb: &KnowledgeBaseInfo{ID: "kb", Name: "n", Profile: &types.KnowledgeBaseProfile{
				Gist: long, Topics: []string{long}, TypicalQuestions: []string{long},
			}},
			contains: []string{
				"<gist>" + strings.Repeat("长", 200) + "...</gist>",
				"<topics>" + strings.Repeat("长", 150) + "...</topics>",
				"<question>" + strings.Repeat("长", 100) + "...</question>",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text := formatKnowledgeBaseList([]*KnowledgeBaseInfo{tt.kb})
			for _, s := range tt.contains {
				require.Contains(t, text, s)
			}
			for _, s := range tt.notContains {
				require.NotContains(t, text, s)
			}
		})
	}
}

func TestFormatKnowledgeBaseListOmitsEmptyProfile(t *testing.T) {
	text := formatKnowledgeBaseList([]*KnowledgeBaseInfo{
		{ID: "kb-1", Name: "Ops", Profile: &types.KnowledgeBaseProfile{Status: types.KnowledgeBaseProfileStatusEmpty}},
		{ID: "kb-2", Name: "Docs"},
	})
	require.NotContains(t, text, "<generated_profile>")
}
