package github

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/scm"
)

func (h *Host) EnsurePRComment(ctx context.Context, pr *scm.PR, body string) error {
	selector, err := prSelector(pr)
	if err != nil {
		return err
	}
	args := append([]string{"pr", "view", selector}, h.repoArgs()...)
	args = append(args, "--json", "comments")
	out, err := h.cmd(ctx, "gh", args...).Output()
	if err != nil {
		return fmt.Errorf("read PR comments: %w", err)
	}
	var response struct {
		Comments *[]struct {
			Body string `json:"body"`
		} `json:"comments"`
	}
	if err := json.Unmarshal(out, &response); err != nil || response.Comments == nil {
		return fmt.Errorf("read PR comments: incomplete or malformed response")
	}
	for _, comment := range *response.Comments {
		if comment.Body == body {
			return nil
		}
	}
	args = append([]string{"pr", "comment", selector}, h.repoArgs()...)
	cmd := h.cmd(ctx, "gh", append(args, "--body-file", "-")...)
	cmd.Stdin = strings.NewReader(body)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("publish PR comment: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}
