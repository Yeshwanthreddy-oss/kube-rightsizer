package gitops

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/kushagrasikka/kube-rightsizer/internal/patch"
)

const fixtureDeployment = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: checkout-api
  namespace: shop
spec:
  replicas: 3
  template:
    spec:
      containers:
        - name: app
          image: ghcr.io/shop/checkout-api:1.4.2
          resources:
            requests:
              cpu: 250m
              memory: 256Mi
            limits:
              cpu: 500m
              memory: 512Mi
`

// newLocalRemote creates a real (non-bare) git repository on disk,
// pre-populated with one manifest file committed to "main". It stands in
// for a GitHub remote in tests: go-git can clone from and push to a plain
// local path just like a real remote, so this exercises the actual git
// plumbing without any network access.
func newLocalRemote(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()

	repo, err := git.PlainInitWithOptions(dir, &git.PlainInitOptions{
		InitOptions: git.InitOptions{DefaultBranch: plumbing.NewBranchReferenceName("main")},
	})
	if err != nil {
		t.Fatalf("PlainInitWithOptions: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	for name, content := range files {
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write fixture file: %v", err)
		}
		if _, err := wt.Add(name); err != nil {
			t.Fatalf("git add %s: %v", name, err)
		}
	}
	_, err = wt.Commit("initial commit", &git.CommitOptions{
		Author: &object.Signature{Name: "fixture", Email: "fixture@localhost", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("initial commit: %v", err)
	}

	// go-git can only push to a non-bare local repo if it isn't currently
	// checked out on the branch being pushed; converting it to look like
	// a bare repo (by pushing into refs directly) is what our Push code
	// exercises. To keep the fixture realistic we instead allow non-fast
	// -forward local pushes by relying on distinct branch names per test
	// (new branch != main), which go-git permits without needing
	// --force or a bare repository.
	return dir
}

func TestRepository_PrepareBranch_AppliesPatchAndCommits(t *testing.T) {
	remote := newLocalRemote(t, map[string]string{"deploy/checkout-api.yaml": fixtureDeployment})

	repo := &Repository{CloneURL: remote, AuthorName: "tester", AuthorEmail: "tester@localhost"}
	dir, err := repo.PrepareBranch(context.Background(), "main", "rightsizer/checkout-api-2026-07", "chore: right-size checkout-api", []FileChange{
		{Path: "deploy/checkout-api.yaml", Patches: []patch.ContainerPatch{
			{Container: "app", CPURequest: "300m", MemoryRequest: "300Mi"},
		}},
	})
	if err != nil {
		t.Fatalf("PrepareBranch: %v", err)
	}
	defer func() { _ = repo.Cleanup(dir) }()

	patched, err := os.ReadFile(filepath.Join(dir, "deploy/checkout-api.yaml"))
	if err != nil {
		t.Fatalf("reading patched file: %v", err)
	}
	if !contains(string(patched), "300m") || !contains(string(patched), "300Mi") {
		t.Fatalf("patched file missing expected values:\n%s", patched)
	}

	// A commit should exist on the new branch with our message.
	r, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatalf("PlainOpen: %v", err)
	}
	head, err := r.Head()
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if head.Name().Short() != "rightsizer/checkout-api-2026-07" {
		t.Fatalf("HEAD branch = %s, want rightsizer/checkout-api-2026-07", head.Name().Short())
	}
	commit, err := r.CommitObject(head.Hash())
	if err != nil {
		t.Fatalf("CommitObject: %v", err)
	}
	if commit.Message != "chore: right-size checkout-api" {
		t.Fatalf("commit message = %q, want %q", commit.Message, "chore: right-size checkout-api")
	}
}

func TestRepository_PrepareBranch_NoChangesReturnsErrNoChanges(t *testing.T) {
	remote := newLocalRemote(t, map[string]string{"deploy/checkout-api.yaml": fixtureDeployment})
	repo := &Repository{CloneURL: remote}
	// Patch requesting exactly the values already present -> no diff.
	_, err := repo.PrepareBranch(context.Background(), "main", "rightsizer/no-op", "chore: no-op", []FileChange{
		{Path: "deploy/checkout-api.yaml", Patches: []patch.ContainerPatch{
			{Container: "app", CPURequest: "250m", MemoryRequest: "256Mi", CPULimit: "500m", MemoryLimit: "512Mi"},
		}},
	})
	if err != ErrNoChanges {
		t.Fatalf("expected ErrNoChanges, got %v", err)
	}
}

func TestRepository_PrepareBranch_MissingFileErrors(t *testing.T) {
	remote := newLocalRemote(t, map[string]string{"deploy/checkout-api.yaml": fixtureDeployment})
	repo := &Repository{CloneURL: remote}
	_, err := repo.PrepareBranch(context.Background(), "main", "rightsizer/missing", "msg", []FileChange{
		{Path: "deploy/does-not-exist.yaml", Patches: []patch.ContainerPatch{{Container: "app", CPURequest: "1m"}}},
	})
	if err == nil {
		t.Fatal("expected error for missing manifest file")
	}
}

func TestRepository_PushAndManager_OpensPRAgainstFakeOpener(t *testing.T) {
	remote := newLocalRemote(t, map[string]string{"deploy/checkout-api.yaml": fixtureDeployment})
	repo := &Repository{CloneURL: remote}
	opener := &FakePullRequestOpener{}
	mgr := &Manager{Repo: repo, Opener: opener, Owner: "shop", Name: "gitops-manifests"}

	pr, err := mgr.OpenRecommendationPR(context.Background(), "main", "rightsizer/checkout-api", "chore: right-size checkout-api",
		"Right-size checkout-api", "p95-based recommendation", []string{"rightsizer"},
		[]FileChange{{Path: "deploy/checkout-api.yaml", Patches: []patch.ContainerPatch{{Container: "app", CPURequest: "310m"}}}})
	if err != nil {
		t.Fatalf("OpenRecommendationPR: %v", err)
	}
	if pr.Number != 1 {
		t.Fatalf("pr.Number = %d, want 1", pr.Number)
	}
	if pr.Branch != "rightsizer/checkout-api" {
		t.Fatalf("pr.Branch = %q, want rightsizer/checkout-api", pr.Branch)
	}
	if len(opener.Calls) != 1 {
		t.Fatalf("expected 1 OpenPullRequest call, got %d", len(opener.Calls))
	}
	call := opener.Calls[0]
	if call.Owner != "shop" || call.Repo != "gitops-manifests" || call.Base != "main" || call.Head != "rightsizer/checkout-api" {
		t.Fatalf("unexpected call: %+v", call)
	}

	// The pushed branch should now exist on the "remote" (the local
	// fixture repo), proving Push actually talked to it.
	remoteRepo, err := git.PlainOpen(remote)
	if err != nil {
		t.Fatalf("PlainOpen(remote): %v", err)
	}
	ref, err := remoteRepo.Reference(plumbing.NewBranchReferenceName("rightsizer/checkout-api"), true)
	if err != nil {
		t.Fatalf("expected pushed branch on remote, got error: %v", err)
	}
	if ref.Hash().IsZero() {
		t.Fatal("pushed branch ref has zero hash")
	}
}

func TestManager_OpenRecommendationPR_NoChangesPropagatesError(t *testing.T) {
	remote := newLocalRemote(t, map[string]string{"deploy/checkout-api.yaml": fixtureDeployment})
	repo := &Repository{CloneURL: remote}
	opener := &FakePullRequestOpener{}
	mgr := &Manager{Repo: repo, Opener: opener, Owner: "shop", Name: "gitops-manifests"}

	_, err := mgr.OpenRecommendationPR(context.Background(), "main", "rightsizer/no-op", "msg", "title", "body", nil,
		[]FileChange{{Path: "deploy/checkout-api.yaml", Patches: []patch.ContainerPatch{
			{Container: "app", CPURequest: "250m", MemoryRequest: "256Mi", CPULimit: "500m", MemoryLimit: "512Mi"},
		}}})
	if err == nil {
		t.Fatal("expected error propagated from PrepareBranch")
	}
	if len(opener.Calls) != 0 {
		t.Fatalf("PR should not have been opened when there were no changes, got %d calls", len(opener.Calls))
	}
}

func TestFakePullRequestOpener_ErrOverride(t *testing.T) {
	opener := &FakePullRequestOpener{Err: context.DeadlineExceeded}
	_, err := opener.OpenPullRequest(context.Background(), "o", "r", "main", "head", "t", "b", nil)
	if err != context.DeadlineExceeded {
		t.Fatalf("expected overridden error, got %v", err)
	}
}

func TestSplitOwnerRepo(t *testing.T) {
	owner, repo, err := SplitOwnerRepo("shop/gitops-manifests")
	if err != nil || owner != "shop" || repo != "gitops-manifests" {
		t.Fatalf("SplitOwnerRepo = %q, %q, %v", owner, repo, err)
	}
	if _, _, err := SplitOwnerRepo("invalid"); err == nil {
		t.Fatal("expected error for repo string with no slash")
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(haystack); i++ {
			if haystack[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
