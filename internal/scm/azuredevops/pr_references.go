package azuredevops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func (h *Host) LinkPRWorkItems(ctx context.Context, pr *scm.PR, ids []string) error {
	id := h.prID(pr)
	if id == "" {
		return errors.New("link work items: missing PR id")
	}
	if len(ids) == 0 {
		return nil
	}
	args := append([]string{"repos", "pr", "work-item", "add", "--id", id, "--work-items"}, ids...)
	args = append(args, h.orgArgs()...)
	_, err := outputJSON(h.cmd(ctx, "az", append(args, "--output", "json")...))
	return err
}

func (h *Host) EnsurePRComment(ctx context.Context, pr *scm.PR, body string) error {
	id := h.prID(pr)
	if id == "" || h.project == "" || h.repo == "" {
		return errors.New("publish PR comment: missing PR or repository identity")
	}
	args := []string{"devops", "invoke", "--area", "git", "--resource", "pullRequestThreads", "--route-parameters", "project=" + h.project, "repositoryId=" + h.repo, "pullRequestId=" + id}
	args = append(args, h.orgArgs()...)
	args = append(args, "--api-version", "7.1", "--output", "json")
	out, err := outputJSON(h.cmd(ctx, "az", args...))
	if err != nil {
		return err
	}
	var threads struct {
		Value *[]struct {
			Comments []struct {
				Content string `json:"content"`
			} `json:"comments"`
		} `json:"value"`
	}
	if err := json.Unmarshal(out, &threads); err != nil || threads.Value == nil {
		return fmt.Errorf("read PR comments: incomplete or malformed response")
	}
	for _, thread := range *threads.Value {
		for _, comment := range thread.Comments {
			if comment.Content == body {
				return nil
			}
		}
	}
	payload, err := json.Marshal(map[string]any{"comments": []map[string]any{{"parentCommentId": 0, "content": body, "commentType": 1}}, "status": 4})
	if err != nil {
		return err
	}
	file, err := os.CreateTemp("", "no-mistakes-pr-comment-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(payload); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	args = append(args, "--http-method", "POST", "--in-file", file.Name(), "--encoding", "utf-8")
	_, err = outputJSON(h.cmd(ctx, "az", args...))
	return err
}
