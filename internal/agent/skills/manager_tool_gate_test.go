package skills

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func TestManagerTenantSkillToolRequirements(t *testing.T) {
	rows := []*types.TenantSkillEntity{
		{
			Name:         "sdk-platform-query",
			Description:  "查询线上实时配置",
			Instructions: "使用 curl 查询平台配置",
			Enabled:      true,
			Status:       types.SkillStatusReady,
		},
		{
			Name:         "sdk-operation-guide",
			Description:  "操作指引模板",
			Instructions: "## 核心结论",
			Enabled:      true,
			Status:       types.SkillStatusReady,
		},
	}

	t.Run("tenant persisted required_tools enforced live via AvailableToolsFunc without zip download", func(t *testing.T) {
		bundleLoads := 0
		persistedRows := []*types.TenantSkillEntity{{
			Name:          "custom-ops",
			Description:   "Custom ops",
			Instructions:  "# Ops\nRun ops.",
			RequiredTools: types.SkillRequiredTools{"read_file", "mcp_ops_*"},
			Enabled:       true,
			Status:        types.SkillStatusReady,
			BundleSHA256:  "sha-ops",
		}}
		tools := []string{"search_knowledge"}
		mgr := NewManager(&ManagerConfig{
			Enabled:            true,
			AvailableToolsFunc: func() []string { return tools },
		}, nil).WithTenantSource(NewTenantSkillSource(persistedRows, func(_ *types.TenantSkillEntity) ([]byte, error) {
			bundleLoads++
			return nil, os.ErrNotExist
		}))

		require.NoError(t, mgr.Initialize(t.Context()))
		require.Empty(t, mgr.GetAllMetadata())
		require.False(t, mgr.IsSkillAvailable("custom-ops"))
		require.False(t, mgr.IsSkillAvailable("nonexistent-skill"))
		require.Zero(t, bundleLoads, "Initialize/GetAllMetadata must not download tenant ZIP bundles")
		_, _, err := mgr.PrepareShellEnvironment(t.Context(), "s1", "custom-ops", "echo hi", nil)
		require.Error(t, err)

		// Register required tools dynamically AFTER Initialize: live GetAllMetadata & IsSkillAvailable reflect them without Reload.
		tools = append(tools, "read_file", "mcp_ops_query")
		require.Len(t, mgr.GetAllMetadata(), 1)
		require.Equal(t, "custom-ops", mgr.GetAllMetadata()[0].Name)
		require.True(t, mgr.IsSkillAvailable("custom-ops"))
		require.False(t, mgr.IsSkillAvailable("nonexistent-skill"))
		require.Zero(t, bundleLoads)
	})

	t.Run("exposed and loadable when shell is available", func(t *testing.T) {
		mgr := NewManager(&ManagerConfig{
			Enabled:        true,
			AvailableTools: []string{"ask_user", "edit_sandbox_file", "query_knowledge_graph", "read_document", "read_file", "search_knowledge", "shell_exec", "shell_exec_async", "web_fetch", "web_search", "wiki_read_page", "wiki_search", "work_cancel", "work_wait", "write_sandbox_file"},
		}, nil).WithTenantSource(NewTenantSkillSource(rows, nil))

		require.NoError(t, mgr.Initialize(t.Context()))
		meta := mgr.GetAllMetadata()
		require.Len(t, meta, 2)

		skill, err := mgr.LoadSkill(t.Context(), "sdk-platform-query")
		require.NoError(t, err)
		require.Equal(t, "sdk-platform-query", skill.Name)
	})
}

func TestManagerGatesYAMLRequiredTools(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "db-inspector")
	require.NoError(t, os.MkdirAll(skillDir, 0o755))
	content := "---\nname: db-inspector\ndescription: Inspect DB\nrequired_tools:\n  - database_query|mcp_*\n---\n# Body\nRun queries.\n"
	require.NoError(t, os.WriteFile(filepath.Join(skillDir, SkillFileName), []byte(content), 0o644))

	mgrBlocked := NewManager(&ManagerConfig{
		SkillDirs:      []string{dir},
		AvailableTools: []string{"search_knowledge"},
		Enabled:        true,
	}, nil)
	require.NoError(t, mgrBlocked.Initialize(t.Context()))
	require.Empty(t, mgrBlocked.GetAllMetadata())
	_, err := mgrBlocked.LoadSkill(t.Context(), "db-inspector")
	require.Error(t, err)
	require.NoError(t, mgrBlocked.Reload(t.Context()))
	require.Empty(t, mgrBlocked.GetAllMetadata())

	mgrAllowed := NewManager(&ManagerConfig{
		SkillDirs:      []string{dir},
		AvailableTools: []string{"database_query"},
		Enabled:        true,
	}, nil)
	require.NoError(t, mgrAllowed.Initialize(t.Context()))
	require.Len(t, mgrAllowed.GetAllMetadata(), 1)
	skill, err := mgrAllowed.LoadSkill(t.Context(), "db-inspector")
	require.NoError(t, err)
	require.Equal(t, "db-inspector", skill.Name)
}
