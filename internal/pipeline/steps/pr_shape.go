package steps

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/safepath"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const prGeneratorLine = "🤖 Generated with [Claude Code](https://claude.com/claude-code)"
const fullIntentCommentNote = "Full intent is in the first PR comment."

func legacyWhatChanged(body string) string {
	body = strings.TrimSpace(stripGeneratedSections(body))
	body = strings.TrimSpace(strings.ReplaceAll(body, prGeneratorLine, ""))
	if strings.HasPrefix(body, "## Summary") {
		body = strings.Replace(body, "## Summary", "## What Changed", 1)
	}
	if !strings.HasPrefix(body, "## What Changed") {
		body = "## What Changed\n\n" + body
	}
	return joinBlocks(body, prGeneratorLine)
}

func runWorkItems(sctx *pipeline.StepContext) []string {
	if sctx == nil {
		return nil
	}
	branch := ""
	if sctx.Run != nil {
		branch = sctx.Run.Branch
	}
	return scm.ExtractWorkItems(publicPRIntent(sctx), branch)
}

func runWorkItemTitle(sctx *pipeline.StepContext, title string) string {
	ids := runWorkItems(sctx)
	if len(ids) > 1 {
		ids = ids[:1]
	}
	return scm.WorkItemTitle(title, ids)
}

func legacyPipelineSummary(steps []*db.StepResult, rounds map[string][]*db.StepRound, headSHA string, policy pipelineAttestationPolicy) string {
	var lines []string
	for _, step := range steps {
		if step == nil {
			continue
		}
		line, _ := buildStepEntry(step, rounds[step.ID], prBodyHTML)
		line = strings.ReplaceAll(line, "**", "")
		line = strings.Replace(line, stepDisplayName(step.StepName)+" -", string(step.StepName)+" -", 1)
		if separator := strings.Index(line, " "); separator >= 0 {
			marker := "⚠️"
			if step.Status == types.StepStatusCompleted && strings.HasPrefix(line, "✅") {
				marker = "✅"
			}
			line = marker + line[separator:]
		}
		lines = append(lines, "- "+neutralizeAttestationMarkers(line))
	}
	if len(lines) == 0 {
		return ""
	}
	return "## Pipeline\n\n" + noMistakesPRSignature + "\n\n" + buildPipelineAttestationWithPolicy(steps, rounds, headSHA, policy) + "\n\n" + strings.Join(lines, "\n")
}

func compactIntentAtSentence(intent string, budget int, measure func(string) int) string {
	if measure(intent) <= budget {
		return intent
	}
	last := 0
	used := 0
	openQuote := false
	openCurlyQuote := false
	escaped := false
	sentenceEnd := false
	lastEnd := 0
	for offset, character := range intent {
		end := offset + len(string(character))
		if unicode.IsSpace(character) && sentenceEnd && !openQuote && !openCurlyQuote {
			last = offset
		}
		used += measure(string(character))
		if used > budget {
			break
		}
		if character == '"' && !escaped {
			openQuote = !openQuote
			if openQuote {
				sentenceEnd = false
			}
		} else if character == '“' {
			openCurlyQuote = true
			sentenceEnd = false
		} else if character == '”' {
			openCurlyQuote = false
		} else if !unicode.IsSpace(character) {
			sentenceEnd = strings.ContainsRune(".!?。！？", character)
		}
		escaped = character == '\\' && !escaped
		lastEnd = end
	}
	if lastEnd == len(intent) && sentenceEnd && !openQuote && !openCurlyQuote {
		last = lastEnd
	}
	return strings.TrimSpace(intent[:last])
}

func fitPRCore(sctx *pipeline.StepContext, whatChanged, risk, pipelineMD string, limit int) string {
	return fitPRCoreMeasured(sctx, whatChanged, risk, pipelineMD, limit, scm.PRBodyLen, scm.ClampPRBody, clampPipelineSectionWithinLimit)
}

func fitPRCoreMeasured(sctx *pipeline.StepContext, whatChanged, risk, pipelineMD string, limit int, measure func(string) int, clamp, clampPipeline func(string, int) string) string {
	whatChanged = neutralizeAttestationMarkers(whatChanged)
	risk = neutralizeAttestationMarkers(risk)
	intent := ""
	if publicPRIntent(sctx) != "" {
		intent = "## Intent\n\n" + fullIntentCommentNote
	}
	header := pipelineSectionHeader(pipelineMD)
	narrative := strings.TrimPrefix(strings.TrimSpace(strings.ReplaceAll(whatChanged, prGeneratorLine, "")), "## What Changed")
	fixed := joinBlocks(intent, "## What Changed", prGeneratorLine, "## Risk Assessment", header)
	budget := limit - measure(fixed) - 8
	if budget < 0 {
		return clamp(fixed, limit)
	}
	if measure(risk) > budget/3 {
		risk = clamp(risk, budget/3)
	}
	remaining := budget - measure(risk)
	if measure(narrative) > remaining/2 {
		narrative = clamp(narrative, remaining/2)
	}
	prefix := joinBlocks(intent, "## What Changed\n\n"+strings.TrimSpace(narrative), prGeneratorLine, "## Risk Assessment\n\n"+risk)
	pipelineBudget := limit - measure(prefix) - 2
	return joinBlocks(prefix, clampPipeline(pipelineMD, pipelineBudget))
}

func githubWorkItemLine(sctx *pipeline.StepContext, body string) string {
	var references []string
	present := map[string]bool{}
	for _, id := range scm.ExtractWorkItems(body) {
		present[id] = true
	}
	for _, id := range runWorkItems(sctx) {
		if !present[id] {
			references = append(references, "AB#"+id)
		}
	}
	if len(references) == 0 {
		return ""
	}
	return "- Related work items: " + strings.Join(references, ", ")
}

func ensureGitHubWorkItems(sctx *pipeline.StepContext, body string) string {
	line := githubWorkItemLine(sctx, body)
	if line == "" {
		return body
	}
	if strings.Contains(body, prGeneratorLine) {
		return strings.Replace(body, prGeneratorLine, line+"\n\n"+prGeneratorLine, 1)
	}
	return joinBlocks(body, line)
}

func fitPRIntent(sctx *pipeline.StepContext, sections string, limit int, measure func(string) int) string {
	intent := neutralizeAttestationMarkers(publicPRIntent(sctx))
	full := prependIntentSection(sections, sctx)
	if limit <= 0 || measure(full) <= limit || intent == "" {
		return full
	}
	budget := limit - measure("## Intent\n\n"+fullIntentCommentNote+"\n\n"+sections+"\n\n")
	short := compactIntentAtSentence(intent, budget, measure)
	return joinBlocks("## Intent\n\n"+joinBlocks(short, fullIntentCommentNote), sections)
}

func publishPRReferences(sctx *pipeline.StepContext, host scm.Host, pr *scm.PR, body string) error {
	if strings.Contains(body, fullIntentCommentNote) && publicPRIntent(sctx) != "" {
		publisher, ok := host.(scm.PRCommentPublisher)
		if !ok {
			return fmt.Errorf("provider cannot publish the full intent comment")
		}
		comment := "## Full intent\n\n" + safepath.RedactText(neutralizeAttestationMarkers(publicPRIntent(sctx)))
		if err := publisher.EnsurePRComment(sctx.Ctx, pr, comment); err != nil {
			return fmt.Errorf("publish full intent comment: %w", err)
		}
	}
	if linker, ok := host.(scm.PRWorkItemLinker); ok {
		if ids := runWorkItems(sctx); len(ids) > 0 {
			if err := linker.LinkPRWorkItems(sctx.Ctx, pr, ids); err != nil {
				return fmt.Errorf("link PR work items: %w", err)
			}
		}
	}
	return nil
}
