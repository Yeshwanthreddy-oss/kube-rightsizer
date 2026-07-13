// Package controller implements the ResourcePolicy reconcile loop: for
// every Deployment a policy selects, pull historical usage from
// Prometheus, compute a p95-based recommendation, and -- if the change is
// large enough to matter -- open a GitOps pull request rather than
// mutating the live object.
package controller

import (
	"context"
	"fmt"
	"math"
	"path"
	"sort"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	rightsizerv1alpha1 "github.com/kushagrasikka/kube-rightsizer/api/v1alpha1"
	"github.com/kushagrasikka/kube-rightsizer/internal/gitops"
	"github.com/kushagrasikka/kube-rightsizer/internal/metrics"
	"github.com/kushagrasikka/kube-rightsizer/internal/patch"
	"github.com/kushagrasikka/kube-rightsizer/internal/recommend"
)

// defaultRequeueInterval is how often an active (non-suspended) policy is
// re-evaluated. Usage history changes slowly, so this deliberately does not
// need to be fast.
const defaultRequeueInterval = 15 * time.Minute

// PRManager is the subset of gitops.Manager the controller depends on. It
// exists so tests can inject a manager built from a local git fixture and a
// FakePullRequestOpener instead of talking to a real GitHub remote.
type PRManager interface {
	OpenRecommendationPR(ctx context.Context, base, branch, commitMessage, prTitle, prBody string, labels []string, changes []gitops.FileChange) (*gitops.PullRequest, error)
}

// ResourcePolicyReconciler reconciles a ResourcePolicy object.
type ResourcePolicyReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// UsageSource is where historical CPU/memory samples come from. In
	// production this is a *metrics.PrometheusSource; tests inject a
	// *metrics.FakeSource.
	UsageSource metrics.UsageSource

	// NewPRManager builds the GitOps PR manager for a given policy. Tests
	// inject a manager wired to a local git fixture + FakePullRequestOpener;
	// production wires a real gitops.Repository + GitHubPullRequestOpener.
	// If GitOps is not configured on the policy this is never called.
	NewPRManager func(policy *rightsizerv1alpha1.ResourcePolicy, target *rightsizerv1alpha1.GitOpsTarget) (PRManager, error)

	// Clock allows tests to control "now" for deterministic branch names
	// and status timestamps.
	Clock func() time.Time
}

// +kubebuilder:rbac:groups=rightsizer.slate.dev,resources=resourcepolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rightsizer.slate.dev,resources=resourcepolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile implements the ResourcePolicy control loop described in the
// package doc.
func (r *ResourcePolicyReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	now := r.now()

	var policy rightsizerv1alpha1.ResourcePolicy
	if err := r.Get(ctx, req.NamespacedName, &policy); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("fetching ResourcePolicy: %w", err)
	}

	if policy.Spec.Suspend {
		logger.Info("resource policy suspended, skipping", "policy", req.NamespacedName)
		return ctrl.Result{}, nil
	}

	deployments, err := r.listSelectedDeployments(ctx, &policy)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("listing deployments: %w", err)
	}

	window, err := ParseWindow(orDefault(policy.Spec.Window, "7d"))
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("invalid spec.window: %w", err)
	}
	minSamples := int(policy.Spec.MinSamples)
	if minSamples <= 0 {
		minSamples = 10
	}
	changeThreshold := float64(policy.Spec.ChangeThresholdPercent)
	if changeThreshold <= 0 {
		changeThreshold = 10
	}

	excluded := buildExclusionSet(policy.Spec.Exclusions)

	var recommendations []rightsizerv1alpha1.ContainerRecommendation
	// changesByWorkload accumulates every container patch destined for the
	// same workload's manifest so a single PR/commit covers the whole
	// Deployment instead of one PR per container.
	changesByWorkload := map[string][]patch.ContainerPatch{}

	for _, dep := range deployments {
		for _, c := range dep.Spec.Template.Spec.Containers {
			if excluded[exclusionKey{workload: dep.Name, container: c.Name}] || excluded[exclusionKey{workload: dep.Name, container: ""}] {
				continue
			}

			rec, changed, err := r.evaluateContainer(ctx, &policy, dep.Namespace, dep.Name, c, window, minSamples, changeThreshold)
			if err != nil {
				if _, ok := err.(*recommend.ErrInsufficientSamples); ok {
					logger.Info("skipping container, insufficient usage history", "workload", dep.Name, "container", c.Name, "err", err.Error())
					continue
				}
				if _, ok := err.(*metrics.ErrNoData); ok {
					logger.Info("skipping container, no usage data", "workload", dep.Name, "container", c.Name)
					continue
				}
				return ctrl.Result{}, fmt.Errorf("evaluating %s/%s: %w", dep.Name, c.Name, err)
			}
			recommendations = append(recommendations, *rec)

			if changed != nil {
				changesByWorkload[dep.Name] = append(changesByWorkload[dep.Name], *changed)
			}
		}
	}

	sort.Slice(recommendations, func(i, j int) bool {
		if recommendations[i].Workload != recommendations[j].Workload {
			return recommendations[i].Workload < recommendations[j].Workload
		}
		return recommendations[i].Container < recommendations[j].Container
	})

	var lastPRURL string
	if policy.Spec.GitOps != nil && len(changesByWorkload) > 0 {
		lastPRURL, err = r.openPullRequests(ctx, &policy, changesByWorkload, now)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("opening gitops pull requests: %w", err)
		}
	}

	policy.Status.ObservedGeneration = policy.Generation
	t := metav1.NewTime(now)
	policy.Status.LastReconcileTime = &t
	policy.Status.Recommendations = recommendations
	if lastPRURL != "" {
		policy.Status.LastPullRequestURL = lastPRURL
	}
	setReadyCondition(&policy, policy.Generation)

	if err := r.Status().Update(ctx, &policy); err != nil {
		return ctrl.Result{}, fmt.Errorf("updating status: %w", err)
	}

	return ctrl.Result{RequeueAfter: defaultRequeueInterval}, nil
}

// evaluateContainer computes the CPU+memory recommendation for one
// container and, if the request changed by more than changeThresholdPercent
// on either dimension, returns a non-nil patch.ContainerPatch to apply.
func (r *ResourcePolicyReconciler) evaluateContainer(
	ctx context.Context,
	policy *rightsizerv1alpha1.ResourcePolicy,
	namespace, workload string,
	c corev1.Container,
	window time.Duration,
	minSamples int,
	changeThresholdPercent float64,
) (*rightsizerv1alpha1.ContainerRecommendation, *patch.ContainerPatch, error) {
	q := metrics.Query{Namespace: namespace, Workload: workload, Container: c.Name, Window: window}

	cpuSamples, err := r.UsageSource.CPUMillicoreSamples(ctx, q)
	if err != nil {
		return nil, nil, err
	}
	memSamples, err := r.UsageSource.MemoryByteSamples(ctx, q)
	if err != nil {
		return nil, nil, err
	}

	cpuRec, err := recommend.Recommend(cpuSamples, policy.Spec.CPU, minSamples, recommend.ResourceCPU)
	if err != nil {
		return nil, nil, err
	}
	memRec, err := recommend.Recommend(memSamples, policy.Spec.Memory, minSamples, recommend.ResourceMemory)
	if err != nil {
		return nil, nil, err
	}

	currentCPU := float64(c.Resources.Requests.Cpu().MilliValue())
	currentMem := float64(c.Resources.Requests.Memory().Value())

	cpuChangePct := recommend.ChangePercent(currentCPU, cpuRec.Request)
	memChangePct := recommend.ChangePercent(currentMem, memRec.Request)
	maxChangePct := cpuChangePct
	if memChangePct > maxChangePct {
		maxChangePct = memChangePct
	}

	rec := &rightsizerv1alpha1.ContainerRecommendation{
		Workload:              workload,
		Container:             c.Name,
		CurrentCPURequest:     c.Resources.Requests.Cpu().String(),
		CurrentMemRequest:     c.Resources.Requests.Memory().String(),
		RecommendedCPURequest: fmt.Sprintf("%dm", roundInt64(cpuRec.Request)),
		RecommendedCPULimit:   fmt.Sprintf("%dm", roundInt64(cpuRec.Limit)),
		RecommendedMemRequest: fmt.Sprintf("%d", roundInt64(memRec.Request)),
		RecommendedMemLimit:   fmt.Sprintf("%d", roundInt64(memRec.Limit)),
		SampleCount:           int32(minInt(len(cpuSamples), len(memSamples))),
		ChangePercent:         int32(roundInt64(maxChangePct)),
	}

	if maxChangePct < changeThresholdPercent {
		return rec, nil, nil
	}

	return rec, &patch.ContainerPatch{
		Container:     c.Name,
		CPURequest:    fmt.Sprintf("%dm", roundInt64(cpuRec.Request)),
		CPULimit:      fmt.Sprintf("%dm", roundInt64(cpuRec.Limit)),
		MemoryRequest: fmt.Sprintf("%d", roundInt64(memRec.Request)),
		MemoryLimit:   fmt.Sprintf("%d", roundInt64(memRec.Limit)),
	}, nil
}

// roundInt64 rounds to the nearest integer rather than truncating, so a
// recommendation of e.g. 919.99 millicores becomes the intended 920m
// instead of silently under-provisioning by a rounding artifact.
func roundInt64(f float64) int64 {
	return int64(math.Round(f))
}

// openPullRequests opens one PR per workload that had a material
// recommendation change, returning the URL of the last one opened (mainly
// useful in single-workload-per-policy setups and for tests).
func (r *ResourcePolicyReconciler) openPullRequests(
	ctx context.Context,
	policy *rightsizerv1alpha1.ResourcePolicy,
	changesByWorkload map[string][]patch.ContainerPatch,
	now time.Time,
) (string, error) {
	target := policy.Spec.GitOps
	mgr, err := r.NewPRManager(policy, target)
	if err != nil {
		return "", fmt.Errorf("building gitops manager: %w", err)
	}

	manifestDir := target.ManifestPath
	if manifestDir == "" {
		manifestDir = "."
	}
	base := target.BaseBranch
	if base == "" {
		base = "main"
	}

	var lastURL string
	workloads := make([]string, 0, len(changesByWorkload))
	for w := range changesByWorkload {
		workloads = append(workloads, w)
	}
	sort.Strings(workloads)

	for _, workload := range workloads {
		patches := changesByWorkload[workload]
		// Convention: one manifest file per Deployment, named
		// "<workload>.yaml" under GitOps.ManifestPath. This keeps path
		// resolution a pure function of policy config (safe to unit
		// test) instead of requiring a network round trip to search the
		// repo before we know what to clone.
		manifestPath := path.Join(manifestDir, workload+".yaml")
		branch := fmt.Sprintf("rightsizer/%s-%d", workload, policy.Generation)
		commitMsg := fmt.Sprintf("chore(rightsizer): right-size %s\n\nGenerated by kube-rightsizer from %s usage history.", workload, orDefault(policy.Spec.Window, "7d"))
		title := fmt.Sprintf("Right-size %s (kube-rightsizer)", workload)
		body := buildPRBody(workload, patches)

		pr, err := mgr.OpenRecommendationPR(ctx, base, branch, commitMsg, title, body, target.PRLabels, []gitops.FileChange{
			{Path: manifestPath, Patches: patches},
		})
		if err != nil {
			if err == gitops.ErrNoChanges {
				continue
			}
			return lastURL, fmt.Errorf("workload %s: %w", workload, err)
		}
		lastURL = pr.URL
	}
	return lastURL, nil
}

func buildPRBody(workload string, patches []patch.ContainerPatch) string {
	var b strings.Builder
	fmt.Fprintf(&b, "kube-rightsizer computed the following p95-based resource recommendation for **%s**:\n\n", workload)
	fmt.Fprintf(&b, "| Container | CPU request | CPU limit | Memory request | Memory limit |\n")
	fmt.Fprintf(&b, "|---|---|---|---|---|\n")
	for _, p := range patches {
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n", p.Container, p.CPURequest, p.CPULimit, p.MemoryRequest, p.MemoryLimit)
	}
	b.WriteString("\nThis PR was opened automatically. It does not modify any live cluster object -- review and merge to apply.\n")
	return b.String()
}

// listSelectedDeployments returns the Deployments a policy applies to: all
// Deployments in policy.Namespace, filtered by WorkloadSelector if set.
func (r *ResourcePolicyReconciler) listSelectedDeployments(ctx context.Context, policy *rightsizerv1alpha1.ResourcePolicy) ([]appsv1.Deployment, error) {
	var list appsv1.DeploymentList
	opts := []client.ListOption{client.InNamespace(policy.Namespace)}
	if policy.Spec.WorkloadSelector != nil {
		sel, err := metav1.LabelSelectorAsSelector(policy.Spec.WorkloadSelector)
		if err != nil {
			return nil, fmt.Errorf("invalid workloadSelector: %w", err)
		}
		opts = append(opts, client.MatchingLabelsSelector{Selector: sel})
	}
	if err := r.List(ctx, &list, opts...); err != nil {
		return nil, err
	}
	return list.Items, nil
}

type exclusionKey struct {
	workload, container string
}

func buildExclusionSet(exclusions []rightsizerv1alpha1.ContainerExclusion) map[exclusionKey]bool {
	set := make(map[exclusionKey]bool, len(exclusions))
	for _, e := range exclusions {
		set[exclusionKey{workload: e.Workload, container: e.Container}] = true
	}
	return set
}

func setReadyCondition(policy *rightsizerv1alpha1.ResourcePolicy, generation int64) {
	cond := metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionTrue,
		Reason:             "Reconciled",
		Message:            "policy reconciled successfully",
		ObservedGeneration: generation,
		LastTransitionTime: metav1.Now(),
	}
	for i, existing := range policy.Status.Conditions {
		if existing.Type == cond.Type {
			if existing.Status == cond.Status {
				cond.LastTransitionTime = existing.LastTransitionTime
			}
			policy.Status.Conditions[i] = cond
			return
		}
	}
	policy.Status.Conditions = append(policy.Status.Conditions, cond)
}

func (r *ResourcePolicyReconciler) now() time.Time {
	if r.Clock != nil {
		return r.Clock()
	}
	return time.Now()
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// SetupWithManager wires the reconciler into a controller-runtime Manager.
// Deployments are read-only inputs (never mutated, per the safety-first
// design), so this deliberately only watches ResourcePolicy itself;
// re-evaluation on a fixed interval (defaultRequeueInterval) is what picks
// up drifting usage rather than a Deployment watch.
func (r *ResourcePolicyReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&rightsizerv1alpha1.ResourcePolicy{}).
		Complete(r)
}
