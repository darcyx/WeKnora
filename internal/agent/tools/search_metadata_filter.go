package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
)

// narrowTargetsByMetadata restricts every target to the documents whose
// custom metadata carries all of the wanted key=value pairs. A target keeps
// its own document and tag scope; targets left with no document are dropped.
//
// ponytail: lists every document of each base and matches in memory; push the
// filter into a JSON query on custom_metadata when bases reach tens of
// thousands of documents.
func (t *SearchKnowledgeTool) narrowTargetsByMetadata(
	ctx context.Context, targets types.SearchTargets, want map[string]string,
) (types.SearchTargets, error) {
	matchedByKB := make(map[string][]string)
	var narrowed types.SearchTargets
	for _, target := range targets {
		if target == nil {
			continue
		}
		matched, ok := matchedByKB[target.KnowledgeBaseID]
		if !ok {
			// Documents live under the base owner, which differs from the
			// caller's tenant for organization-shared bases.
			listCtx := ctx
			if target.TenantID != 0 {
				listCtx = context.WithValue(ctx, types.TenantIDContextKey, target.TenantID)
			}
			docs, err := t.knowledgeService.ListKnowledgeByKnowledgeBaseID(listCtx, target.KnowledgeBaseID)
			if err != nil {
				return nil, fmt.Errorf("list documents of %s: %w", target.KnowledgeBaseID, err)
			}
			for _, doc := range docs {
				if doc != nil && metadataMatches(doc.CustomMetadata, want) {
					matched = append(matched, doc.ID)
				}
			}
			matchedByKB[target.KnowledgeBaseID] = matched
		}
		if next := restrictTarget(target, matched); next != nil {
			narrowed = append(narrowed, next)
		}
	}
	return narrowed, nil
}

// restrictTarget returns a copy of target limited to the matched documents,
// or nil when none of them is in the target's scope.
func restrictTarget(target *types.SearchTarget, matched []string) *types.SearchTarget {
	ids := matched
	if len(target.KnowledgeIDs) > 0 {
		ids = nil
		for _, id := range matched {
			if slices.Contains(target.KnowledgeIDs, id) {
				ids = append(ids, id)
			}
		}
	}
	if len(ids) == 0 {
		return nil
	}
	next := *target
	next.Type = types.SearchTargetTypeKnowledge
	next.KnowledgeIDs = ids
	return &next
}

// metadataMatches reports whether custom carries every wanted key with an
// equal value, ignoring case. A list value matches when any element does.
func metadataMatches(custom types.JSON, want map[string]string) bool {
	var values map[string]any
	if len(custom) == 0 || json.Unmarshal(custom, &values) != nil {
		return false
	}
	for key, expected := range want {
		got, ok := values[strings.TrimSpace(key)]
		if !ok || !metadataValueEquals(got, strings.TrimSpace(expected)) {
			return false
		}
	}
	return true
}

func metadataValueEquals(got any, expected string) bool {
	if list, ok := got.([]any); ok {
		return slices.ContainsFunc(list, func(v any) bool { return metadataValueEquals(v, expected) })
	}
	return got != nil && strings.EqualFold(strings.TrimSpace(fmt.Sprint(got)), expected)
}
