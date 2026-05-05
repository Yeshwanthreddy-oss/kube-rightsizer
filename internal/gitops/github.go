package gitops

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/go-github/v63/github"
)

// GitHubPullRequestOpener is the real PullRequestOpener implementation,
// backed by the GitHub REST API. It is never exercised in unit tests
// (which use FakePullRequestOpener instead) since it requires network
// access and a token.
type GitHubPullRequestOpener struct {
	client *github.Client
}

// NewGitHubPullRequestOpener builds a GitHubPullRequestOpener authenticated
// with a personal access token or GitHub App installation token.
func NewGitHubPullRequestOpener(token string) *GitHubPullRequestOpener {
	return &GitHubPullRequestOpener{client: github.NewClient(nil).WithAuthToken(token)}
}

func (g *GitHubPullRequestOpener) OpenPullRequest(ctx context.Context, owner, repo, base, head, title, body string, labels []string) (*PullRequest, error) {
	pr, _, err := g.client.PullRequests.Create(ctx, owner, repo, &github.NewPullRequest{
		Title: github.String(title),
		Base:  github.String(base),
		Head:  github.String(head),
		Body:  github.String(body),
	})
	if err != nil {
		return nil, fmt.Errorf("creating pull request %s/%s %s->%s: %w", owner, repo, head, base, err)
	}

	if len(labels) > 0 {
		if _, _, err := g.client.Issues.AddLabelsToIssue(ctx, owner, repo, pr.GetNumber(), labels); err != nil {
			return nil, fmt.Errorf("adding labels to PR #%d: %w", pr.GetNumber(), err)
		}
	}

	return &PullRequest{URL: pr.GetHTMLURL(), Number: pr.GetNumber(), Branch: head}, nil
}

var _ PullRequestOpener = (*GitHubPullRequestOpener)(nil)

// SplitOwnerRepo splits a "owner/repo" GitOpsTarget.Repo string. Returns an
// error if the string does not contain exactly one "/".
func SplitOwnerRepo(ownerRepo string) (owner, repo string, err error) {
	parts := strings.SplitN(ownerRepo, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid repo %q, want \"owner/name\"", ownerRepo)
	}
	return parts[0], parts[1], nil
}
