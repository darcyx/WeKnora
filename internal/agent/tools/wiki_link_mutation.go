package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/application/repository"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

const wikiRollbackTimeout = 10 * time.Second

func isWikiNotFoundError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, repository.ErrWikiPageNotFound) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "not found")
}

type wikiContentRewrite func(string) (string, bool)

type appliedWikiContentChange struct {
	page            *types.WikiPage
	originalContent string
}

// applyIncomingWikiContentRewrite updates machine-maintained links without
// incrementing user-visible page versions. It stops on the first failure and
// returns the already-applied changes so the caller can compensate.
func applyIncomingWikiContentRewrite(
	ctx context.Context,
	service interfaces.WikiPageService,
	kbID string,
	inLinks []string,
	rewrite wikiContentRewrite,
) ([]appliedWikiContentChange, []string, error) {
	var changes []appliedWikiContentChange
	var updatedSlugs []string
	for _, sourceSlug := range dedupNonEmptyStrings(inLinks) {
		page, err := service.GetPageBySlug(ctx, kbID, sourceSlug)
		if err != nil {
			// Stale/dirty InLinks pointing at already-deleted pages should not
			// hard-abort a rename or delete; skip missing referrers.
			if isWikiNotFoundError(err) {
				continue
			}
			return changes, updatedSlugs, fmt.Errorf("load incoming page %s: %w", sourceSlug, err)
		}
		if page == nil {
			continue
		}
		updatedContent, changed := rewrite(page.Content)
		if !changed {
			continue
		}
		before := types.WithWikiPageWriteSnapshot(ctx, page)
		original := page.Content
		page.Content = updatedContent
		if err := service.UpdateAutoLinkedContent(before, page); err != nil {
			page.Content = original
			return changes, updatedSlugs, fmt.Errorf("update incoming page %s: %w", sourceSlug, err)
		}
		changes = append(changes, appliedWikiContentChange{page: page, originalContent: original})
		updatedSlugs = append(updatedSlugs, sourceSlug)
	}
	return changes, updatedSlugs, nil
}

func rollbackWikiContentChanges(
	ctx context.Context,
	service interfaces.WikiPageService,
	changes []appliedWikiContentChange,
) error {
	if len(changes) == 0 {
		return nil
	}
	rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), wikiRollbackTimeout)
	defer cancel()
	var failures []string
	for i := len(changes) - 1; i >= 0; i-- {
		change := changes[i]
		guarded := types.WithWikiPageWriteSnapshot(rollbackCtx, change.page)
		change.page.Content = change.originalContent
		if err := service.UpdateAutoLinkedContent(guarded, change.page); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", change.page.Slug, err))
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("failed to roll back incoming pages: %s", strings.Join(failures, "; "))
	}
	return nil
}

func joinWikiMutationErrors(primary error, extras ...error) string {
	parts := []string{primary.Error()}
	for _, err := range extras {
		if err != nil {
			parts = append(parts, err.Error())
		}
	}
	return strings.Join(parts, "; ")
}
