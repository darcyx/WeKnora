package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeSourceRefsWikiService struct {
	interfaces.WikiPageService
	existing *types.WikiPage
	saved    *types.WikiPage
	snapshot *types.WikiPage
}

func (f *fakeSourceRefsWikiService) GetPageBySlug(context.Context, string, string) (*types.WikiPage, error) {
	cp := *f.existing
	return &cp, nil
}

func (f *fakeSourceRefsWikiService) UpdatePage(ctx context.Context, p *types.WikiPage) (*types.WikiPage, error) {
	f.snapshot, _ = types.WikiPageWriteSnapshotFromContext(ctx)
	f.saved = p
	return p, nil
}

type fakeSourceRefsKnowledgeService struct {
	interfaces.KnowledgeService
	rows      map[string]*types.Knowledge
	gotTenant uint64
}

func (f *fakeSourceRefsKnowledgeService) GetKnowledgeBatch(
	_ context.Context, tenantID uint64, ids []string,
) ([]*types.Knowledge, error) {
	f.gotTenant = tenantID
	var out []*types.Knowledge
	for _, id := range ids {
		if k, ok := f.rows[id]; ok {
			out = append(out, k)
		}
	}
	return out, nil
}

func TestUpdatePageSourceRefs(t *testing.T) {
	rows := map[string]*types.Knowledge{
		"k1": {ID: "k1", Title: "Doc One"},
		"k2": {ID: "k2", FileName: "two.pdf"},
	}
	existingRefs := types.StringArray{"old|Old"}
	tests := []struct {
		name     string
		body     string
		wantCode int
		wantRefs types.StringArray
	}{
		{"absent keeps refs", `{"title":"T"}`, http.StatusOK, existingRefs},
		{"empty clears", `{"source_refs":[]}`, http.StatusOK, types.StringArray{}},
		{
			"normalises dedups and rebuilds titles",
			`{"source_refs":["k1|client title"," k1 ","","|x","k2"]}`,
			http.StatusOK, types.StringArray{"k1|Doc One", "k2|two.pdf"},
		},
		{"unknown id rejected", `{"source_refs":["k1","missing"]}`, http.StatusBadRequest, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			wiki := &fakeSourceRefsWikiService{existing: &types.WikiPage{
				Slug: "concept/a", Title: "T", Version: 3, ParentSlug: "concept/parent", SourceRefs: existingRefs,
			}}
			know := &fakeSourceRefsKnowledgeService{rows: rows}
			h := &WikiPageHandler{wikiService: wiki, kbService: fakeWikiPageTitlesKBService{}, knowledgeSvc: know}

			gin.SetMode(gin.TestMode)
			r := gin.New()
			r.PUT("/knowledgebase/:kb_id/wiki/pages/*slug", func(c *gin.Context) {
				c.Set(types.TenantIDContextKey.String(), uint64(42))
				h.UpdatePage(c)
			})
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPut, "/knowledgebase/kb-1/wiki/pages/concept/a",
				strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			r.ServeHTTP(rec, req)

			require.Equal(t, tt.wantCode, rec.Code, rec.Body.String())
			if tt.wantCode != http.StatusOK {
				assert.Nil(t, wiki.saved)
				return
			}
			require.NotNil(t, wiki.saved)
			require.NotNil(t, wiki.snapshot)
			assert.Equal(t, "concept/parent", wiki.snapshot.ParentSlug)
			assert.Equal(t, tt.wantRefs, wiki.saved.SourceRefs)
			if strings.Contains(tt.body, "k1") {
				assert.Equal(t, uint64(42), know.gotTenant)
			}
		})
	}
}
