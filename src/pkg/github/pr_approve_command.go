package github

import (
	"context"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// replyPRApproveCommand answers at most one unanswered command per PR per
// pass. PRs never enter the issue unpark path: an approval cannot clear their
// hold labels. The merge engine explains eligibility without authorizing a
// merge. Replies share the issue command's durable, author-checked ID marker.
func (c *Client) replyPRApproveCommand(ctx context.Context, displayRepo, owner, repo string, issue *gh.Issue, cutoff time.Time, reply func(context.Context, string, int, *gh.IssueComment) (string, error)) (bool, error) {
	comments, err := c.listIssueComments(ctx, owner, repo, issue.GetNumber())
	if err != nil {
		return false, err
	}
	answered := c.answeredUnparkCommentIDs(comments)
	for _, cm := range comments {
		if cm == nil || cm.GetID() <= 0 || answered[cm.GetID()] || strings.EqualFold(cm.GetUser().GetLogin(), c.AppBotLogin()) || cm.GetCreatedAt().Time.Before(cutoff) || !IsReviewHoldReleaseCommand(cm.GetBody()) {
			continue
		}
		body, err := reply(ctx, displayRepo, issue.GetNumber(), cm)
		if err != nil {
			return false, err
		}
		if body == "" {
			continue
		}
		if err := c.postUnparkReply(ctx, owner, repo, issue.GetNumber(), cm.GetID(), body); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}
