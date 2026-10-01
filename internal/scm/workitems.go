package scm

import (
	"context"
	"regexp"
	"strings"
)

var workItemReference = regexp.MustCompile(`(?i)\bAB#([1-9][0-9]*)\b`)
var emptyWorkItemSuffix = regexp.MustCompile(`\([\s,]*\)`)

func ExtractWorkItems(texts ...string) []string {
	var ids []string
	seen := map[string]bool{}
	for _, text := range texts {
		for _, match := range workItemReference.FindAllStringSubmatch(text, -1) {
			if !seen[match[1]] {
				seen[match[1]] = true
				ids = append(ids, match[1])
			}
		}
	}
	return ids
}

func WorkItemTitle(title string, ids []string) string {
	if len(ids) == 0 {
		return title
	}
	var references []string
	for _, id := range ids {
		references = append(references, "AB#"+id)
	}
	title = strings.TrimSpace(workItemReference.ReplaceAllString(title, ""))
	title = emptyWorkItemSuffix.ReplaceAllString(title, "")
	title = strings.Join(strings.Fields(title), " ")
	return title + " (" + strings.Join(references, ", ") + ")"
}

type PRCommentPublisher interface {
	EnsurePRComment(context.Context, *PR, string) error
}

type PRWorkItemLinker interface {
	LinkPRWorkItems(context.Context, *PR, []string) error
}
