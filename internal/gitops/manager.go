package gitops

import (
	"context"
	"fmt"
)

// Manager ties together a git Repository and a forge PullRequestOpener
// into the single operation the controller actually needs: "turn these
// manifest patches into a reviewable pull request".
type Manager struct {
	Repo   *Repository
	Opener PullRequestOpener
	Owner  string
	Name   string
}

// OpenRecommendationPR clones base, applies changes on a new branch,
// pushes it, and opens a pull request back to base. The local worktree is
// always cleaned up, even on error.
func (m *Manager) OpenRecommendationPR(ctx context.Context, base, branch, commitMessage, prTitle, prBody string, labels []string, changes []FileChange) (*PullRequest, error) {
	dir, err := m.Repo.PrepareBranch(ctx, base, branch, commitMessage, changes)
	if err != nil {
		return nil, fmt.Errorf("preparing branch: %w", err)
	}
	defer func() { _ = m.Repo.Cleanup(dir) }()

	if err := m.Repo.Push(ctx, dir, branch); err != nil {
		return nil, fmt.Errorf("pushing branch: %w", err)
	}

	pr, err := m.Opener.OpenPullRequest(ctx, m.Owner, m.Name, base, branch, prTitle, prBody, labels)
	if err != nil {
		return nil, fmt.Errorf("opening pull request: %w", err)
	}
	return pr, nil
}
