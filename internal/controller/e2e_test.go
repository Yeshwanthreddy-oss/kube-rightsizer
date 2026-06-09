package controller

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	rightsizerv1alpha1 "github.com/kushagrasikka/kube-rightsizer/api/v1alpha1"
	"github.com/kushagrasikka/kube-rightsizer/internal/gitops"
	"github.com/kushagrasikka/kube-rightsizer/internal/metrics"
)

const e2eManifest = `apiVersion: apps/v1
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

// TestReconcile_EndToEnd_OpensRealLocalGitOpsPR is the full pipeline test:
// a fake Prometheus source feeds usage samples, the reconciler computes a
// p95 recommendation, patches a real (local, non-bare) git repository on a
// new branch, pushes it, and asks a FakePullRequestOpener to open a PR --
// exercising every seam except the real GitHub API call, entirely offline.
func TestReconcile_EndToEnd_OpensRealLocalGitOpsPR(t *testing.T) {
	scheme := newScheme(t)
	dep := newDeployment("shop", "checkout-api", "250m", "256Mi")

	remoteDir := t.TempDir()
	repo, err := git.PlainInitWithOptions(remoteDir, &git.PlainInitOptions{
		InitOptions: git.InitOptions{DefaultBranch: plumbing.NewBranchReferenceName("main")},
	})
	if err != nil {
		t.Fatalf("PlainInitWithOptions: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree: %v", err)
	}
	manifestRelPath := filepath.Join("deploy", "checkout-api.yaml")
	full := filepath.Join(remoteDir, manifestRelPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, []byte(e2eManifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if _, err := wt.Add(manifestRelPath); err != nil {
		t.Fatalf("git add: %v", err)
	}
	if _, err := wt.Commit("initial", &git.CommitOptions{
		Author: &object.Signature{Name: "fixture", Email: "fixture@localhost", When: time.Now()},
	}); err != nil {
		t.Fatalf("commit: %v", err)
	}

	policy := &rightsizerv1alpha1.ResourcePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "shop", Generation: 3},
		Spec: rightsizerv1alpha1.ResourcePolicySpec{
			Window: "7d", MinSamples: 5, ChangeThresholdPercent: 10,
			CPU:    rightsizerv1alpha1.ResourceThresholds{Percentile: 95, HeadroomPercent: 15, LimitMultiplier: "2"},
			Memory: rightsizerv1alpha1.ResourceThresholds{Percentile: 95, HeadroomPercent: 15, LimitMultiplier: "2"},
			GitOps: &rightsizerv1alpha1.GitOpsTarget{
				Repo:         "shop/gitops-manifests",
				BaseBranch:   "main",
				ManifestPath: "deploy",
				PRLabels:     []string{"rightsizer"},
			},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dep, policy).
		WithStatusSubresource(&rightsizerv1alpha1.ResourcePolicy{}).Build()

	usage := metrics.NewFakeSource()
	usage.SetCPU("shop", "checkout-api", "app", constantSamples(30, 800))              // way above 250m
	usage.SetMemory("shop", "checkout-api", "app", constantSamples(30, 700*1024*1024)) // way above 256Mi

	fakeOpener := &gitops.FakePullRequestOpener{}
	r := &ResourcePolicyReconciler{
		Client: c, Scheme: scheme, UsageSource: usage,
		NewPRManager: func(p *rightsizerv1alpha1.ResourcePolicy, target *rightsizerv1alpha1.GitOpsTarget) (PRManager, error) {
			owner, name, err := gitops.SplitOwnerRepo(target.Repo)
			if err != nil {
				return nil, err
			}
			return &gitops.Manager{
				Repo:   &gitops.Repository{CloneURL: remoteDir, AuthorName: "kube-rightsizer", AuthorEmail: "bot@localhost"},
				Opener: fakeOpener,
				Owner:  owner,
				Name:   name,
			}, nil
		},
	}

	_, err = r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(policy)})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if len(fakeOpener.Calls) != 1 {
		t.Fatalf("expected exactly 1 PR to be opened, got %d: %+v", len(fakeOpener.Calls), fakeOpener.Calls)
	}
	call := fakeOpener.Calls[0]
	if call.Owner != "shop" || call.Repo != "gitops-manifests" || call.Base != "main" {
		t.Fatalf("unexpected PR call: %+v", call)
	}
	if len(call.Labels) != 1 || call.Labels[0] != "rightsizer" {
		t.Fatalf("expected rightsizer label, got %v", call.Labels)
	}

	// Verify the branch was really pushed to the local "remote" with the
	// patched manifest content -- proving the whole chain (fake metrics ->
	// recommend -> yaml patch -> git commit -> git push) executed for
	// real, not mocked away.
	remoteRepo, err := git.PlainOpen(remoteDir)
	if err != nil {
		t.Fatalf("PlainOpen(remote): %v", err)
	}
	ref, err := remoteRepo.Reference(plumbing.NewBranchReferenceName(call.Head), true)
	if err != nil {
		t.Fatalf("expected pushed branch %q on remote: %v", call.Head, err)
	}
	commit, err := remoteRepo.CommitObject(ref.Hash())
	if err != nil {
		t.Fatalf("CommitObject: %v", err)
	}
	fileObj, err := commit.File(manifestRelPath)
	if err != nil {
		t.Fatalf("reading patched file from pushed commit: %v", err)
	}
	content, err := fileObj.Contents()
	if err != nil {
		t.Fatalf("file contents: %v", err)
	}
	if !contains(content, "920m") { // 800 * 1.15 = 920
		t.Fatalf("expected patched cpu request ~920m in pushed manifest, got:\n%s", content)
	}

	var updated rightsizerv1alpha1.ResourcePolicy
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(policy), &updated); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if updated.Status.LastPullRequestURL == "" {
		t.Fatal("expected LastPullRequestURL to be set after opening a PR")
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
