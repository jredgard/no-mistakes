// Package azuredevops implements scm.Host backed by the az CLI with the
// azure-devops extension.
package azuredevops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

// outputJSON runs cmd and returns its stdout alone, leaving stderr out of the
// payload so non-JSON az chatter (preview-command notices, token-refresh
// messages) cannot corrupt the bytes a caller json.Unmarshal's. On failure it
// surfaces the separately-captured stderr in the error.
func outputJSON(cmd *exec.Cmd) ([]byte, error) {
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(bytes.TrimSpace(ee.Stderr)) > 0 {
			return nil, fmt.Errorf("%s: %w", strings.TrimSpace(string(ee.Stderr)), err)
		}
		return nil, err
	}
	return out, nil
}

// clampDescription truncates body to Azure DevOps' PR-description cap. The
// pipeline already budgets the body to fit (shedding whole sections), so this
// is the connector-level backstop that guarantees `az repos pr create`/`update`
// never sees an over-length description, no matter how the body was produced.
func clampDescription(body string) string {
	return scm.ClampPRBody(body, scm.MaxPRBodyChars(scm.ProviderAzureDevOps))
}

// CmdFactory builds an exec.Cmd in the caller's workdir with the caller's env.
type CmdFactory func(ctx context.Context, name string, args ...string) *exec.Cmd

// Host talks to Azure DevOps through the az CLI (azure-devops extension).
type Host struct {
	cmd          CmdFactory
	cliAvailable func() bool
	org          string // organization URL, e.g. https://dev.azure.com/myorg
	project      string // project name (may contain spaces)
	repo         string // repository name
	draft        bool   // open created PRs as drafts (az repos pr create --draft true)
}

// New builds a Host. cliAvailable reports whether the az binary is resolvable
// on the caller's PATH. org is the organization URL; it is passed via
// --organization to every command so they resolve the right organization
// regardless of the process working directory. The daemon runs from a fixed,
// non-repo working dir, so without it az cannot infer the org (or repo) and
// fails on every poll. project and repo name the repository.
func New(cmd CmdFactory, cliAvailable func() bool, org, project, repo string) *Host {
	return &Host{
		cmd:          cmd,
		cliAvailable: cliAvailable,
		org:          strings.TrimSpace(org),
		project:      strings.TrimSpace(project),
		repo:         strings.TrimSpace(repo),
	}
}

// NewWithDraft builds a Host that opens created PRs as drafts when draft is
// true (az repos pr create --draft true). See New for the other parameters.
func NewWithDraft(cmd CmdFactory, cliAvailable func() bool, org, project, repo string, draft bool) *Host {
	h := New(cmd, cliAvailable, org, project, repo)
	h.draft = draft
	return h
}

func (h *Host) Provider() scm.Provider { return scm.ProviderAzureDevOps }

// Capabilities reports the Azure DevOps feature matrix. Merge status is
// reliably available from `az repos pr show`. Failed build logs use the build
// timeline and logs REST resources through the authenticated az devops invoke.
func (h *Host) Capabilities() scm.Capabilities {
	return scm.Capabilities{MergeableState: true, FailedCheckLogs: true}
}

// orgArgs scopes a command to the organization. The show/update/policy-list
// commands accept only --organization because the PR id is organization-unique;
// passing --project/--repository to them is rejected by az.
func (h *Host) orgArgs() []string {
	if h.org == "" {
		return nil
	}
	return []string{"--organization", h.org}
}

// scopeArgs fully scopes a command to org/project/repo. The create and list
// commands need all three to resolve the repository.
func (h *Host) scopeArgs() []string {
	args := h.orgArgs()
	if h.project != "" {
		args = append(args, "--project", h.project)
	}
	if h.repo != "" {
		args = append(args, "--repository", h.repo)
	}
	return args
}

func (h *Host) Available(ctx context.Context) error {
	if h.cliAvailable != nil && !h.cliAvailable() {
		return errors.New("az CLI is not installed")
	}
	// The azure-devops extension is separate from the az binary; without it
	// every `az repos`/`az devops` command fails. Probe it for a clear message.
	if err := h.cmd(ctx, "az", "extension", "show", "--name", "azure-devops").Run(); err != nil {
		return errors.New("az azure-devops extension is not installed (run: az extension add --name azure-devops)")
	}
	// Auth probe: an organization-scoped read exercises the PAT
	// (AZURE_DEVOPS_EXT_PAT, or `az devops login`) against this organization.
	args := []string{"devops", "project", "list", "--query", "value[0].id", "--output", "tsv"}
	args = append(args, h.orgArgs()...)
	if err := h.cmd(ctx, "az", args...).Run(); err != nil {
		return errors.New("az CLI is not authenticated for Azure DevOps")
	}
	return nil
}

func (h *Host) FindPR(ctx context.Context, branch, base string) (*scm.PR, error) {
	args := []string{"repos", "pr", "list", "--source-branch", branch, "--status", "active"}
	if strings.TrimSpace(base) != "" {
		args = append(args, "--target-branch", base)
	}
	args = append(args, h.scopeArgs()...)
	args = append(args, "--output", "json")
	out, err := outputJSON(h.cmd(ctx, "az", args...))
	if err != nil {
		return nil, fmt.Errorf("az repos pr list: %w", err)
	}
	trimmed := bytes.TrimSpace(out)
	if len(trimmed) == 0 {
		return nil, errors.New("az repos pr list: parse response: expected array")
	}
	var prs []azPR
	if err := json.Unmarshal(trimmed, &prs); err != nil {
		return nil, fmt.Errorf("az repos pr list: parse response: %w", err)
	}
	if prs == nil {
		return nil, errors.New("az repos pr list: parse response: expected array")
	}
	if len(prs) == 0 {
		return nil, nil
	}
	for i, candidate := range prs {
		if err := h.validateListedPR(candidate); err != nil {
			return nil, fmt.Errorf("az repos pr list: parse response: entry %d: %w", i, err)
		}
	}
	return h.toPR(&prs[0]), nil
}

func (h *Host) validateListedPR(candidate azPR) error {
	if candidate.PullRequestID <= 0 {
		return errors.New("missing positive pullRequestId")
	}
	// az repos pr list often omits repository.webUrl (null in list payloads;
	// the show endpoint supplies it). Organization is already pinned by the
	// list command's --organization flag, so a missing URL is identified by
	// repository.name and repository.project.name instead of a second show
	// call. A nonempty URL is still parsed and must match; names never
	// rescue a malformed URL.
	if candidate.Repository.WebURL != "" {
		org, project, repo, err := parseRepositoryWebURL(candidate.Repository.WebURL)
		if err != nil {
			return err
		}
		if !strings.EqualFold(azureOrganizationName(org), azureOrganizationName(h.org)) {
			return fmt.Errorf("repository organization %q does not match configured organization %q", org, h.org)
		}
		if !strings.EqualFold(project, h.project) {
			return fmt.Errorf("repository project %q does not match configured project %q", project, h.project)
		}
		if !strings.EqualFold(repo, h.repo) {
			return fmt.Errorf("repository name %q does not match configured repository %q", repo, h.repo)
		}
		if name := strings.TrimSpace(candidate.Repository.Name); name != "" && !strings.EqualFold(name, h.repo) {
			return fmt.Errorf("repository metadata name %q does not match configured repository %q", name, h.repo)
		}
		if name := strings.TrimSpace(candidate.Repository.Project.Name); name != "" && !strings.EqualFold(name, h.project) {
			return fmt.Errorf("repository metadata project %q does not match configured project %q", name, h.project)
		}
		return nil
	}
	repo := strings.TrimSpace(candidate.Repository.Name)
	if repo == "" {
		return errors.New("missing repository.name")
	}
	if !strings.EqualFold(repo, h.repo) {
		return fmt.Errorf("repository metadata name %q does not match configured repository %q", repo, h.repo)
	}
	project := strings.TrimSpace(candidate.Repository.Project.Name)
	if project == "" {
		return errors.New("missing repository.project.name")
	}
	if !strings.EqualFold(project, h.project) {
		return fmt.Errorf("repository metadata project %q does not match configured project %q", project, h.project)
	}
	return nil
}

func parseRepositoryWebURL(raw string) (string, string, string, error) {
	trimmed := strings.TrimSpace(raw)
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", "", "", errors.New("missing valid repository.webUrl")
	}
	if !strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https") {
		return "", "", "", errors.New("repository.webUrl must be HTTP")
	}
	if parsed.ForceQuery || parsed.RawQuery != "" || strings.Contains(trimmed, "#") {
		return "", "", "", errors.New("repository.webUrl must not contain query or fragment")
	}
	segments := splitDecodePath(parsed.EscapedPath())
	gitIndex := -1
	for i, segment := range segments {
		if segment == "_git" {
			gitIndex = i
			break
		}
	}
	if gitIndex < 1 || gitIndex+2 != len(segments) {
		return "", "", "", errors.New("repository.webUrl must end at the repository path")
	}
	org, project, repo, ok := ParseRemote(trimmed)
	if !ok {
		return "", "", "", errors.New("missing valid repository.webUrl")
	}
	return org, project, repo, nil
}

func azureOrganizationName(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	host := strings.ToLower(parsed.Hostname())
	if host == "dev.azure.com" {
		parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
		if len(parts) > 0 {
			return parts[0]
		}
	}
	return strings.TrimSuffix(host, ".visualstudio.com")
}

// runWithDescription runs an az PR command whose description is supplied
// out-of-band through a temp file referenced as `--description @<file>`, and
// returns the command's JSON stdout. buildArgs receives the `@<file>` token and
// returns the full az argv, placing that token where --description's value
// belongs. The (clamped) body is written to the file before the command runs
// and the file is always removed afterward.
//
// Why a file instead of passing the body inline as `--description <body>`: on
// Windows the az CLI is a batch shim (az.cmd) that Go executes through cmd.exe.
// cmd.exe terminates each argument at the first newline, so a multi-line body
// was truncated to just its first line (e.g. "## Intent"), silently dropping the
// rest of the PR description - a Windows-only data loss (#501). A file path is a
// single newline-free token that survives cmd.exe intact, and az reads the
// description from the file via the "@" convention (see `az repos pr create
// --help`). The file form additionally sidesteps Python argparse misreading body
// lines that begin with "-"/"--"/"---" (markdown horizontal rules, diff
// "--- a/file" hunks) as new options, which a naive per-line split would break on.
//
// The body is clamped to Azure DevOps' description cap before it is written, so
// az never sees an over-length description no matter how the body was produced.
func (h *Host) runWithDescription(ctx context.Context, body string, buildArgs func(descArg string) []string) ([]byte, error) {
	f, err := os.CreateTemp("", "nm-pr-desc-*.md")
	if err != nil {
		return nil, fmt.Errorf("create PR description temp file: %w", err)
	}
	path := f.Name()
	defer os.Remove(path)
	if _, err := f.WriteString(clampDescription(body)); err != nil {
		f.Close()
		return nil, fmt.Errorf("write PR description temp file: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("close PR description temp file: %w", err)
	}
	return outputJSON(h.cmd(ctx, "az", buildArgs("@"+path)...))
}

func (h *Host) CreatePR(ctx context.Context, branch, base string, content scm.PRContent) (*scm.PR, error) {
	out, err := h.runWithDescription(ctx, content.Body, func(descArg string) []string {
		args := []string{"repos", "pr", "create",
			"--source-branch", branch,
			"--target-branch", base,
			"--title", content.Title,
			"--description", descArg,
		}
		if h.draft {
			args = append(args, "--draft", "true")
		}
		args = append(args, h.scopeArgs()...)
		return append(args, "--output", "json")
	})
	if err != nil {
		return nil, fmt.Errorf("az repos pr create: %w", err)
	}
	var pr azPR
	if err := json.Unmarshal(out, &pr); err != nil {
		return nil, fmt.Errorf("az repos pr create: parse response: %w", err)
	}
	return h.toPR(&pr), nil
}

func (h *Host) UpdatePR(ctx context.Context, pr *scm.PR, content scm.PRContent) (*scm.PR, error) {
	id := h.prID(pr)
	if id == "" {
		return nil, errors.New("az repos pr update: missing PR id")
	}
	if _, err := h.runWithDescription(ctx, content.Body, func(descArg string) []string {
		args := []string{"repos", "pr", "update", "--id", id, "--description", descArg}
		if content.Title != "" {
			args = append(args, "--title", content.Title)
		}
		args = append(args, h.orgArgs()...)
		return append(args, "--output", "json")
	}); err != nil {
		return nil, fmt.Errorf("az repos pr update: %w", err)
	}
	return pr, nil
}

func (h *Host) GetPRState(ctx context.Context, pr *scm.PR) (scm.PRState, error) {
	got, err := h.showPR(ctx, pr)
	if err != nil {
		return "", err
	}
	return normalizePRState(got.Status), nil
}

func (h *Host) GetChecks(ctx context.Context, pr *scm.PR) ([]scm.Check, error) {
	evals, err := h.policyEvaluations(ctx, pr)
	if err != nil {
		return nil, err
	}
	checks := make([]scm.Check, 0, len(evals))
	for _, e := range evals {
		if !e.isCICheck() {
			continue
		}
		bucket := azStatusBucket(e.Status)
		if bucket == "" {
			continue
		}
		providerID := ""
		if id := strings.TrimSpace(e.EvaluationID); id != "" {
			providerID = "azure-policy-evaluation:" + id
		}
		checks = append(checks, scm.Check{
			Name:                 e.checkName(),
			ProviderID:           providerID,
			Bucket:               bucket,
			CompletedAt:          parseAzTime(e.CompletedDate),
			UnstartedPolicyBuild: e.isUnstartedBuild(),
		})
	}
	return checks, nil
}

func (h *Host) policyEvaluations(ctx context.Context, pr *scm.PR) ([]policyEval, error) {
	id := h.prID(pr)
	if id == "" {
		return nil, errors.New("az repos pr policy list: missing PR id")
	}
	args := append([]string{"repos", "pr", "policy", "list", "--id", id}, h.orgArgs()...)
	args = append(args, "--output", "json")
	out, err := outputJSON(h.cmd(ctx, "az", args...))
	if err != nil {
		return nil, fmt.Errorf("az repos pr policy list: %w", err)
	}
	var evals []policyEval
	if err := json.Unmarshal(out, &evals); err != nil {
		return nil, fmt.Errorf("parse policy evaluations: %w", err)
	}
	return evals, nil
}

func (h *Host) GetMergeableState(ctx context.Context, pr *scm.PR) (scm.MergeableState, error) {
	got, err := h.showPR(ctx, pr)
	if err != nil {
		return "", err
	}
	return normalizeMergeableState(got.MergeStatus), nil
}

func (h *Host) FetchFailedCheckLogs(ctx context.Context, pr *scm.PR, branch, headSHA string, failingNames []string) (string, error) {
	targets := make([]scm.CheckTarget, 0, len(failingNames))
	for _, name := range failingNames {
		targets = append(targets, scm.CheckTarget{Name: name})
	}
	logs, err := h.FetchFailedCheckTargetLogs(ctx, pr, branch, headSHA, targets)
	if err != nil {
		return "", err
	}
	return scm.CombineFailedCheckLogs(logs)
}

const maxFailedLogBytes = 32 * 1024

// FetchFailedCheckTargetLogs resolves selected checks from the live PR policy
// context. It deliberately does not infer builds from branch or headSHA: build
// policies can run against Azure's synthetic merge commit.
func (h *Host) FetchFailedCheckTargetLogs(ctx context.Context, pr *scm.PR, _, _ string, targets []scm.CheckTarget) ([]scm.FailedCheckLog, error) {
	if len(targets) == 0 {
		return nil, nil
	}
	evals, err := h.policyEvaluations(ctx, pr)
	if err != nil {
		return nil, err
	}
	results := make([]scm.FailedCheckLog, len(targets))
	budget := max(0, maxFailedLogBytes-2*(len(targets)-1)) / len(targets)
	for i, target := range targets {
		results[i].Target = target
		var outputs []string
		var errs []error
		var builds []policyEval
		for _, e := range evals {
			matches := strings.TrimSpace(target.Name) != "" && e.checkName() == strings.TrimSpace(target.Name)
			if target.ProviderID != "" {
				matches = target.ProviderID == "azure-policy-evaluation:"+strings.TrimSpace(e.EvaluationID)
			}
			if matches && strings.EqualFold(strings.TrimSpace(e.Configuration.Type.DisplayName), "build") && azStatusBucket(e.Status) == scm.CheckBucketFail {
				builds = append(builds, e)
			}
		}
		if len(builds) == 0 {
			results[i].Err = fmt.Errorf("selected Azure DevOps check %q has no failing build policy", target.Identity())
			continue
		}
		for _, e := range builds {
			out, fetchErr := h.failedBuildLogs(ctx, e, budget/len(builds))
			if out != "" {
				outputs = append(outputs, out)
			}
			if fetchErr != nil {
				errs = append(errs, fetchErr)
			}
		}
		results[i].Output = strings.Join(outputs, "")
		results[i].Err = errors.Join(errs...)
	}
	return results, nil
}

type buildTimelineRecord struct {
	ID       string `json:"id"`
	ParentID string `json:"parentId"`
	Type     string `json:"type"`
	Name     string `json:"name"`
	Result   string `json:"result"`
	Log      *struct {
		ID int `json:"id"`
	} `json:"log"`
}

// invokeBuild uses az's existing organization-scoped credentials. Request JSON
// explicitly: invoke cannot print text/plain responses without --out-file, and
// the build log endpoint's JSON representation is {"count":N,"value":[lines]}.
func (h *Host) invokeBuild(ctx context.Context, resource, buildID string, route ...string) ([]byte, error) {
	args := []string{"devops", "invoke", "--area", "build", "--resource", resource, "--route-parameters", "project=" + h.project, "buildId=" + buildID}
	args = append(args, route...)
	args = append(args, h.orgArgs()...)
	args = append(args, "--api-version", "7.1", "--http-method", "GET", "--accept-media-type", "application/json", "--output", "json")
	return outputJSON(h.cmd(ctx, "az", args...))
}

func (h *Host) failedBuildLogs(ctx context.Context, e policyEval, budget int) (string, error) {
	buildID := strings.TrimSpace(fmt.Sprint(e.Context["buildId"]))
	if numeric, ok := e.Context["buildId"].(float64); ok {
		// policyEval.Context uses encoding/json's float64 numbers. fmt.Sprint
		// switches large build IDs to scientific notation, which is not a route ID.
		buildID = strconv.FormatFloat(numeric, 'f', -1, 64)
	}
	id, err := strconv.ParseInt(buildID, 10, 64)
	if err != nil || id <= 0 || h.project == "" {
		return "", fmt.Errorf("Azure DevOps check %q: missing project or positive buildId", e.checkName())
	}
	out, err := h.invokeBuild(ctx, "timeline", buildID)
	if err != nil {
		return "", fmt.Errorf("Azure DevOps build %s timeline: %w", buildID, err)
	}
	var timeline struct {
		Records *[]buildTimelineRecord `json:"records"`
	}
	if err := json.Unmarshal(out, &timeline); err != nil || timeline.Records == nil {
		return "", fmt.Errorf("Azure DevOps build %s: missing or malformed timeline records", buildID)
	}
	byID := make(map[string]buildTimelineRecord)
	var failed []buildTimelineRecord
	seenLogs := make(map[int]bool)
	for _, r := range *timeline.Records {
		byID[r.ID] = r
		if !strings.EqualFold(r.Result, "failed") || (!strings.EqualFold(r.Type, "job") && !strings.EqualFold(r.Type, "task")) || r.Log == nil || r.Log.ID <= 0 || seenLogs[r.Log.ID] {
			continue
		}
		seenLogs[r.Log.ID] = true
		failed = append(failed, r)
	}
	if len(failed) == 0 {
		return "", fmt.Errorf("Azure DevOps build %s: no failed task or job logs in timeline", buildID)
	}
	var outputs []string
	var errs []error
	for _, r := range failed {
		out, err := h.invokeBuild(ctx, "logs", buildID, "logId="+strconv.Itoa(r.Log.ID))
		if err != nil {
			errs = append(errs, fmt.Errorf("Azure DevOps build %s log %d: %w", buildID, r.Log.ID, err))
			continue
		}
		var log struct {
			Value *[]string `json:"value"`
		}
		if err := json.Unmarshal(out, &log); err != nil || log.Value == nil || len(*log.Value) == 0 {
			errs = append(errs, fmt.Errorf("Azure DevOps build %s log %d: missing or malformed log lines", buildID, r.Log.ID))
			continue
		}
		job := r
		for n := 0; !strings.EqualFold(job.Type, "job") && n < len(byID); n++ {
			parent, ok := byID[job.ParentID]
			if !ok {
				break
			}
			job = parent
		}
		header := fmt.Sprintf("=== Check %q / Job %q", e.checkName(), job.Name)
		if strings.EqualFold(r.Type, "task") {
			header += fmt.Sprintf(" / Task %q", r.Name)
		}
		header += fmt.Sprintf(" (build %s, log %d) ===\n", buildID, r.Log.ID)
		outputs = append(outputs, boundedBuildLog(header, strings.Join(*log.Value, "\n"), budget/len(failed)))
	}
	return strings.Join(outputs, ""), errors.Join(errs...)
}

func boundedBuildLog(header, log string, budget int) string {
	if budget <= 0 {
		return ""
	}
	if len(header) >= budget {
		end := budget
		for end > 0 && end < len(header) && !utf8.RuneStart(header[end]) {
			end--
		}
		return header[:end]
	}
	content := strings.TrimSpace(log) + "\n\n"
	remaining := budget - len(header)
	if len(content) > remaining {
		marker := "[... log truncated; showing tail ...]\n"
		if len(marker) > remaining {
			return header
		}
		start := len(content) - (remaining - len(marker))
		for start < len(content) && !utf8.RuneStart(content[start]) {
			start++
		}
		content = marker + content[start:]
	}
	return header + content
}

var _ scm.TargetedFailedCheckLogsHost = (*Host)(nil)

func (h *Host) showPR(ctx context.Context, pr *scm.PR) (*azPR, error) {
	id := h.prID(pr)
	if id == "" {
		return nil, errors.New("az repos pr show: missing PR id")
	}
	args := append([]string{"repos", "pr", "show", "--id", id}, h.orgArgs()...)
	args = append(args, "--output", "json")
	out, err := outputJSON(h.cmd(ctx, "az", args...))
	if err != nil {
		return nil, fmt.Errorf("az repos pr show: %w", err)
	}
	var got azPR
	if err := json.Unmarshal(out, &got); err != nil {
		return nil, fmt.Errorf("parse pull request: %w", err)
	}
	return &got, nil
}

func (h *Host) prID(pr *scm.PR) string {
	if pr == nil {
		return ""
	}
	if id := strings.TrimSpace(pr.Number); id != "" {
		return id
	}
	if num, err := scm.ExtractPRNumber(pr.URL); err == nil {
		return num
	}
	return ""
}

func (h *Host) toPR(raw *azPR) *scm.PR {
	if raw == nil {
		return nil
	}
	id := ""
	if raw.PullRequestID > 0 {
		id = strconv.Itoa(raw.PullRequestID)
	}
	return &scm.PR{
		Number:     id,
		URL:        webPRURL(h.org, h.project, h.repo, "", id),
		BaseBranch: strings.TrimPrefix(strings.TrimSpace(raw.TargetRefName), "refs/heads/"),
	}
}
