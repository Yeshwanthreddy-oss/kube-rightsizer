// Package gitops implements the safety-first alternative to mutating live
// Kubernetes objects: instead of a VPA-style admission patch, recommended
// resource changes are written to a branch of the GitOps manifests repo and
// opened as a pull request for a human (or an auto-merge policy) to approve.
//
// The package is split into two seams so tests never touch the network:
//   - Repository: local git plumbing (clone/branch/commit/push), backed by
//     go-git against a real repo on disk -- tests point it at a local bare
//     repo instead of a remote.
//   - PullRequestOpener: the GitHub (or any forge) API call that turns a
//     pushed branch into a PR -- tests use a fake that just records calls.
package gitops

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	gitshttp "github.com/go-git/go-git/v5/plumbing/transport/http"

	"github.com/kushagrasikka/kube-rightsizer/internal/patch"
)

// FileChange is one manifest file's before/after patch to apply on the
// working tree of a cloned GitOps repo.
type FileChange struct {
	// Path is relative to the repository root.
	Path    string
	Patches []patch.ContainerPatch
}

// PullRequest describes an opened (or would-be, in dry-run) pull request.
type PullRequest struct {
	URL    string
	Number int
	Branch string
}

// PullRequestOpener abstracts the forge-specific "open a PR for this
// pushed branch" call so Repository never has to know about GitHub,
// GitLab, etc. Implementations are expected to be network-calling in
// production and fully faked in tests.
type PullRequestOpener interface {
	OpenPullRequest(ctx context.Context, owner, repo, base, head, title, body string, labels []string) (*PullRequest, error)
}

// Repository wraps a clone of a GitOps manifests repository and knows how
// to apply a set of recommendation patches on a fresh branch, commit them,
// and push.
type Repository struct {
	// CloneURL is passed to go-git PlainClone. For tests this is a local
	// filesystem path (file:// or a bare directory), never a real remote.
	CloneURL string
	// Auth is optional; nil means no auth is configured (fine for local
	// paths and public repos read-only, insufficient for pushing to a
	// real GitHub remote without a token).
	Auth transport.AuthMethod
	// AuthorName/AuthorEmail are used for the generated commit.
	AuthorName  string
	AuthorEmail string
	// Now is overridable in tests for deterministic commit timestamps.
	Now func() time.Time
}

// NewGitHubTokenAuth builds a Repository AuthMethod for HTTPS GitHub access
// using a personal access token / GitHub App installation token.
func NewGitHubTokenAuth(token string) transport.AuthMethod {
	return &gitshttp.BasicAuth{Username: "x-access-token", Password: token}
}

// PrepareBranch clones the repository into a temporary directory, checks
// out a new branch off base, applies each FileChange's patches to the
// matching manifest under root/ManifestPath (resolved by the caller into
// FileChange.Path), and commits the result. It returns the local path of
// the prepared worktree and the branch name; the caller is responsible for
// pushing (via Push) and cleaning up (via Cleanup).
func (r *Repository) PrepareBranch(ctx context.Context, base, branch, commitMessage string, changes []FileChange) (worktreeDir string, err error) {
	if r.Now == nil {
		r.Now = time.Now
	}

	dir, err := os.MkdirTemp("", "kube-rightsizer-gitops-*")
	if err != nil {
		return "", fmt.Errorf("creating temp clone dir: %w", err)
	}
	// Best-effort cleanup on any error path; success path leaves the
	// worktree for the caller (Push/Cleanup take it from here).
	success := false
	defer func() {
		if !success {
			_ = os.RemoveAll(dir)
		}
	}()

	repo, err := git.PlainCloneContext(ctx, dir, false, &git.CloneOptions{
		URL:           r.CloneURL,
		Auth:          r.Auth,
		ReferenceName: plumbing.NewBranchReferenceName(base),
		SingleBranch:  true,
		Depth:         1,
	})
	if err != nil {
		return "", fmt.Errorf("cloning %s: %w", r.CloneURL, err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		return "", fmt.Errorf("getting worktree: %w", err)
	}

	branchRef := plumbing.NewBranchReferenceName(branch)
	head, err := repo.Head()
	if err != nil {
		return "", fmt.Errorf("resolving HEAD: %w", err)
	}
	if err := repo.Storer.SetReference(plumbing.NewHashReference(branchRef, head.Hash())); err != nil {
		return "", fmt.Errorf("creating branch ref %s: %w", branch, err)
	}
	if err := wt.Checkout(&git.CheckoutOptions{Branch: branchRef}); err != nil {
		return "", fmt.Errorf("checking out branch %s: %w", branch, err)
	}

	for _, change := range changes {
		fullPath := filepath.Join(dir, change.Path)
		original, err := os.ReadFile(fullPath)
		if err != nil {
			return "", fmt.Errorf("reading manifest %s: %w", change.Path, err)
		}
		patched, err := patch.ApplyDeploymentPatches(original, change.Patches)
		if err != nil {
			return "", fmt.Errorf("patching manifest %s: %w", change.Path, err)
		}
		if err := os.WriteFile(fullPath, patched, 0o644); err != nil {
			return "", fmt.Errorf("writing patched manifest %s: %w", change.Path, err)
		}
		if _, err := wt.Add(change.Path); err != nil {
			return "", fmt.Errorf("git add %s: %w", change.Path, err)
		}
	}

	status, err := wt.Status()
	if err != nil {
		return "", fmt.Errorf("git status: %w", err)
	}
	if status.IsClean() {
		return "", ErrNoChanges
	}

	authorName := r.AuthorName
	if authorName == "" {
		authorName = "kube-rightsizer"
	}
	authorEmail := r.AuthorEmail
	if authorEmail == "" {
		authorEmail = "kube-rightsizer@localhost"
	}
	_, err = wt.Commit(commitMessage, &git.CommitOptions{
		Author: &object.Signature{
			Name:  authorName,
			Email: authorEmail,
			When:  r.Now(),
		},
	})
	if err != nil {
		return "", fmt.Errorf("committing: %w", err)
	}

	success = true
	return dir, nil
}

// ErrNoChanges is returned by PrepareBranch when applying the requested
// patches did not actually change any tracked file (e.g. the recommendation
// exactly matches what is already committed).
var ErrNoChanges = fmt.Errorf("gitops: no file changes after applying patches")

// Push pushes branch from the local worktree at dir to the configured
// remote.
func (r *Repository) Push(ctx context.Context, dir, branch string) error {
	repo, err := git.PlainOpen(dir)
	if err != nil {
		return fmt.Errorf("opening worktree at %s: %w", dir, err)
	}
	refSpec := config.RefSpec(fmt.Sprintf("refs/heads/%s:refs/heads/%s", branch, branch))
	err = repo.PushContext(ctx, &git.PushOptions{
		RemoteName: "origin",
		Auth:       r.Auth,
		RefSpecs:   []config.RefSpec{refSpec},
	})
	if err != nil && err != git.NoErrAlreadyUpToDate {
		return fmt.Errorf("pushing branch %s: %w", branch, err)
	}
	return nil
}

// Cleanup removes the temporary worktree directory produced by
// PrepareBranch.
func (r *Repository) Cleanup(dir string) error {
	if dir == "" || !strings.Contains(dir, "kube-rightsizer-gitops-") {
		return fmt.Errorf("refusing to remove suspicious directory %q", dir)
	}
	return os.RemoveAll(dir)
}
