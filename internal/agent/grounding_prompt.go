package agent

import (
	"slices"
	"strings"

	"github.com/Tencent/WeKnora/internal/agent/tools"
)

// formatGroundingGuidance lists actual source capabilities; task policy belongs to the base prompt.
func formatGroundingGuidance(names []string) string {
	var b strings.Builder
	if slices.Contains(names, "local_browser") {
		b.WriteString(localBrowserSourcePrompt)
	}
	var kbTools []string
	for _, name := range []string{
		tools.ToolSearchKnowledge, tools.ToolReadDocument, tools.ToolListDocuments,
		tools.ToolWikiSearch, tools.ToolWikiReadPage, tools.ToolQueryKnowledgeGraph,
		tools.ToolDataSchema, tools.ToolDataAnalysis, tools.ToolDatabaseQuery,
	} {
		if slices.Contains(names, name) {
			kbTools = append(kbTools, name)
		}
	}
	if len(kbTools) > 0 {
		b.WriteString("- Available knowledge tools: " + strings.Join(kbTools, ", ") + ".\n")
		b.WriteString("- Route retrieval using the current runtime_context directory, knowledge-base descriptions and generated profiles, within the user's selected scope. Library names are not fixed IDs or evidence; do not probe unrelated libraries to discover the directory.\n")
	}
	for _, name := range []string{tools.ToolWebSearch, tools.ToolWebFetch, tools.ToolDiscoverMCPTools} {
		if slices.Contains(names, name) {
			b.WriteString("- " + name + " is available.\n")
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return "\n\nSource resources:\n" + b.String()
}
