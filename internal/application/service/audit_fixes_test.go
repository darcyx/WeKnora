package service

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/application/repository"
	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestFAQAssistantRequirementsWorksWithRealAgentService(t *testing.T) {
	svc := &sessionService{
		cfg: &config.Config{PromptTemplates: &config.PromptTemplatesConfig{
			AgentSystemPrompt: []config.PromptTemplate{
				{ID: "pure", Mode: "pure", Content: "pure base"},
				{ID: "rag", Mode: "rag", Content: "rag base"},
			},
		}},
		agentService: &agentService{},
	}
	got, ok := svc.faqAssistantRequirements(context.Background(), &types.QARequest{}, &types.AgentConfig{})
	require.True(t, ok, "real *agentService must satisfy faqKBInfoResolver")
	require.Equal(t, "pure base", got)
}

func TestEmptyMCPSelectionModeTreatedAsNone(t *testing.T) {
	cfg := &types.AgentConfig{MCPSelectionMode: ""}
	applyPerRequestMCPScope(context.Background(), cfg, []string{"mcp-1"}, false, []string{"mcp-1"})
	require.Empty(t, cfg.PinnedMCPServiceIDs)

	eff, mode := resolvePerRequestMCPScope([]string{"mcp-1"}, []string{"mcp-1"}, "", false)
	require.Empty(t, eff)
	require.Equal(t, "", mode)
}

func TestInheritedToolOwnsPolicyExemptsCoreNonWriteTools(t *testing.T) {
	for _, name := range []string{
		tools.ToolThinking,
		tools.ToolTodoWrite,
		tools.ToolDataAnalysis,
		tools.ToolDatabaseQuery,
		"request_secure_form",
		"browser_fill_secret",
	} {
		require.True(t, inheritedToolOwnsPolicy(name), "tool %s must be exempt from RiskWrite wrapping", name)
	}
}

func TestWikiPageCreateFillsTenantAndRenamePreservesHistory(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&types.KnowledgeBase{}, &types.WikiPage{}, &types.WikiPageRevision{}, &types.WikiPageIssue{}))

	kb := &types.KnowledgeBase{ID: "kb-wiki", TenantID: 42, Name: "Wiki"}
	require.NoError(t, db.Create(kb).Error)

	repo := repository.NewWikiPageRepository(db)
	svc := NewWikiPageService(repo, nil, nil, nil, nil, nil)

	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, uint64(42))
	// P1 #5: TenantID left 0 on WikiPage must be filled so assertWikiKnowledgeBaseActive succeeds.
	created, err := svc.CreatePage(ctx, &types.WikiPage{
		KnowledgeBaseID: "kb-wiki",
		Slug:            "concept/old-name",
		Title:           "Old Name",
		Content:         "Body linking to self [[concept/old-name]]",
		PageType:        types.WikiPageTypeConcept,
	})
	require.NoError(t, err)
	require.Equal(t, uint64(42), created.TenantID)

	// Add a revision, a child page referencing ParentSlug, and an issue on old slug.
	require.NoError(t, db.Create(&types.WikiPageRevision{
		ID:       "rev-1",
		TenantID: 42,
		PageID:   created.ID,
		Version:  1,
		Title:    "Old Name v1",
		Content:  "Initial body",
	}).Error)
	child, err := svc.CreatePage(ctx, &types.WikiPage{
		KnowledgeBaseID: "kb-wiki",
		Slug:            "concept/child",
		Title:           "Child",
		ParentSlug:      "concept/old-name",
		Content:         "Child body",
		PageType:        types.WikiPageTypeConcept,
	})
	require.NoError(t, err)
	require.NoError(t, db.Create(&types.WikiPageIssue{
		ID:              "issue-1",
		TenantID:        42,
		KnowledgeBaseID: "kb-wiki",
		Slug:            "concept/old-name",
		IssueType:       "other",
		Description:     "needs review",
		Status:          "open",
	}).Error)

	// P1 #6: RenamePage preserves UUID, version, and revision history, rewrites self-links,
	// and cascades child parent_slug + wiki_page_issues.
	renamed, err := svc.RenamePage(ctx, "kb-wiki", "concept/old-name", "concept/new-name")
	require.NoError(t, err)
	require.Equal(t, created.ID, renamed.ID)
	require.Equal(t, created.Version, renamed.Version)
	require.Contains(t, renamed.Content, "[[concept/new-name]]")
	require.NotContains(t, renamed.Content, "[[concept/old-name]]")

	var revCount int64
	require.NoError(t, db.Model(&types.WikiPageRevision{}).Where("page_id = ?", created.ID).Count(&revCount).Error)
	require.EqualValues(t, 1, revCount, "revision history must be preserved on rename")

	reloadedChild, err := svc.GetPageBySlug(ctx, "kb-wiki", child.Slug)
	require.NoError(t, err)
	require.Equal(t, "concept/new-name", reloadedChild.ParentSlug)

	var reloadedIssue types.WikiPageIssue
	require.NoError(t, db.Where("id = ?", "issue-1").First(&reloadedIssue).Error)
	require.Equal(t, "concept/new-name", reloadedIssue.Slug)
}
