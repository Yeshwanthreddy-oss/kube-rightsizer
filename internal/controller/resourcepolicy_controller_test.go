package controller

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	rightsizerv1alpha1 "github.com/kushagrasikka/kube-rightsizer/api/v1alpha1"
	"github.com/kushagrasikka/kube-rightsizer/internal/gitops"
	"github.com/kushagrasikka/kube-rightsizer/internal/metrics"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatalf("appsv1.AddToScheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1.AddToScheme: %v", err)
	}
	if err := rightsizerv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("rightsizerv1alpha1.AddToScheme: %v", err)
	}
	return scheme
}

func newDeployment(namespace, name string, cpuReq, memReq string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "app",
							Image: "example/app:1.0",
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse(cpuReq),
									corev1.ResourceMemory: resource.MustParse(memReq),
								},
							},
						},
					},
				},
			},
		},
	}
}

func constantSamples(n int, v float64) []float64 {
	s := make([]float64, n)
	for i := range s {
		s[i] = v
	}
	return s
}

func TestReconcile_DryRun_NoGitOps_PopulatesStatusOnly(t *testing.T) {
	scheme := newScheme(t)
	dep := newDeployment("shop", "checkout-api", "250m", "256Mi")

	policy := &rightsizerv1alpha1.ResourcePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "shop", Generation: 1},
		Spec: rightsizerv1alpha1.ResourcePolicySpec{
			Window:                 "7d",
			MinSamples:             5,
			ChangeThresholdPercent: 10,
			CPU:                    rightsizerv1alpha1.ResourceThresholds{Percentile: 95, HeadroomPercent: 10, LimitMultiplier: "2"},
			Memory:                 rightsizerv1alpha1.ResourceThresholds{Percentile: 95, HeadroomPercent: 10, LimitMultiplier: "2"},
		},
	}

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(dep, policy).
		WithStatusSubresource(&rightsizerv1alpha1.ResourcePolicy{}).
		Build()

	usage := metrics.NewFakeSource()
	// Usage far above current 250m/256Mi request -> large recommended change.
	usage.SetCPU("shop", "checkout-api", "app", constantSamples(20, 400))
	usage.SetMemory("shop", "checkout-api", "app", constantSamples(20, 400*1024*1024))

	r := &ResourcePolicyReconciler{
		Client:      c,
		Scheme:      scheme,
		UsageSource: usage,
		Clock:       func() time.Time { return time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC) },
	}

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(policy)})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != defaultRequeueInterval {
		t.Fatalf("RequeueAfter = %v, want %v", res.RequeueAfter, defaultRequeueInterval)
	}

	var updated rightsizerv1alpha1.ResourcePolicy
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(policy), &updated); err != nil {
		t.Fatalf("Get updated policy: %v", err)
	}
	if len(updated.Status.Recommendations) != 1 {
		t.Fatalf("expected 1 recommendation, got %d: %+v", len(updated.Status.Recommendations), updated.Status.Recommendations)
	}
	rec := updated.Status.Recommendations[0]
	if rec.Workload != "checkout-api" || rec.Container != "app" {
		t.Fatalf("unexpected recommendation target: %+v", rec)
	}
	if rec.ChangePercent < 10 {
		t.Fatalf("expected material change percent, got %d", rec.ChangePercent)
	}
	if updated.Status.LastPullRequestURL != "" {
		t.Fatalf("no GitOps configured, expected no PR URL, got %q", updated.Status.LastPullRequestURL)
	}
	if updated.Status.ObservedGeneration != 1 {
		t.Fatalf("ObservedGeneration = %d, want 1", updated.Status.ObservedGeneration)
	}
	if len(updated.Status.Conditions) != 1 || updated.Status.Conditions[0].Type != "Ready" {
		t.Fatalf("expected Ready condition, got %+v", updated.Status.Conditions)
	}
}

func TestReconcile_SmallChangeDoesNotTriggerPR(t *testing.T) {
	scheme := newScheme(t)
	// Current request already close to what a stable 250m/256Mi usage
	// history would recommend at 10% headroom -> below the 10% change
	// threshold, so no patch should be generated.
	dep := newDeployment("shop", "checkout-api", "270m", "280Mi")

	policy := &rightsizerv1alpha1.ResourcePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "shop", Generation: 1},
		Spec: rightsizerv1alpha1.ResourcePolicySpec{
			Window: "7d", MinSamples: 5, ChangeThresholdPercent: 10,
			CPU:    rightsizerv1alpha1.ResourceThresholds{Percentile: 95, HeadroomPercent: 10, LimitMultiplier: "2"},
			Memory: rightsizerv1alpha1.ResourceThresholds{Percentile: 95, HeadroomPercent: 10, LimitMultiplier: "2"},
			GitOps: &rightsizerv1alpha1.GitOpsTarget{Repo: "shop/gitops-manifests", ManifestPath: "deploy"},
		},
	}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dep, policy).
		WithStatusSubresource(&rightsizerv1alpha1.ResourcePolicy{}).Build()

	usage := metrics.NewFakeSource()
	usage.SetCPU("shop", "checkout-api", "app", constantSamples(20, 250))
	usage.SetMemory("shop", "checkout-api", "app", constantSamples(20, 256*1024*1024))

	prManagerCalls := 0
	fakeOpener := &gitops.FakePullRequestOpener{}
	r := &ResourcePolicyReconciler{
		Client: c, Scheme: scheme, UsageSource: usage,
		NewPRManager: func(p *rightsizerv1alpha1.ResourcePolicy, target *rightsizerv1alpha1.GitOpsTarget) (PRManager, error) {
			prManagerCalls++
			return &gitops.Manager{Opener: fakeOpener, Owner: "shop", Name: "gitops-manifests"}, nil
		},
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(policy)})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(fakeOpener.Calls) != 0 {
		t.Fatalf("expected no PR to be opened for a below-threshold change, got %d calls", len(fakeOpener.Calls))
	}
	if prManagerCalls != 0 {
		t.Fatalf("NewPRManager should not be called when no workload has a material change, got %d calls", prManagerCalls)
	}
}

func TestReconcile_Suspended_SkipsEntirely(t *testing.T) {
	scheme := newScheme(t)
	policy := &rightsizerv1alpha1.ResourcePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "shop"},
		Spec:       rightsizerv1alpha1.ResourcePolicySpec{Suspend: true},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(policy).
		WithStatusSubresource(&rightsizerv1alpha1.ResourcePolicy{}).Build()

	r := &ResourcePolicyReconciler{Client: c, Scheme: scheme, UsageSource: metrics.NewFakeSource()}
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(policy)})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("suspended policy should not requeue, got %v", res.RequeueAfter)
	}

	var updated rightsizerv1alpha1.ResourcePolicy
	_ = c.Get(context.Background(), client.ObjectKeyFromObject(policy), &updated)
	if updated.Status.LastReconcileTime != nil {
		t.Fatalf("suspended policy should not update status, got %+v", updated.Status)
	}
}

func TestReconcile_NotFound_ReturnsNoError(t *testing.T) {
	scheme := newScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &ResourcePolicyReconciler{Client: c, Scheme: scheme, UsageSource: metrics.NewFakeSource()}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKey{Namespace: "ns", Name: "missing"}})
	if err != nil {
		t.Fatalf("expected no error for a deleted policy, got %v", err)
	}
}

func TestReconcile_ExcludedContainerSkipped(t *testing.T) {
	scheme := newScheme(t)
	dep := newDeployment("shop", "checkout-api", "250m", "256Mi")
	policy := &rightsizerv1alpha1.ResourcePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "shop"},
		Spec: rightsizerv1alpha1.ResourcePolicySpec{
			Window: "7d", MinSamples: 5, ChangeThresholdPercent: 10,
			CPU: rightsizerv1alpha1.ResourceThresholds{Percentile: 95, LimitMultiplier: "2"},
			Exclusions: []rightsizerv1alpha1.ContainerExclusion{
				{Workload: "checkout-api", Container: "app"},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dep, policy).
		WithStatusSubresource(&rightsizerv1alpha1.ResourcePolicy{}).Build()

	usage := metrics.NewFakeSource()
	usage.SetCPU("shop", "checkout-api", "app", constantSamples(20, 400))
	usage.SetMemory("shop", "checkout-api", "app", constantSamples(20, 400*1024*1024))

	r := &ResourcePolicyReconciler{Client: c, Scheme: scheme, UsageSource: usage}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(policy)})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var updated rightsizerv1alpha1.ResourcePolicy
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(policy), &updated); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(updated.Status.Recommendations) != 0 {
		t.Fatalf("expected excluded container to produce no recommendations, got %+v", updated.Status.Recommendations)
	}
}

func TestReconcile_InsufficientSamplesSkipsWithoutError(t *testing.T) {
	scheme := newScheme(t)
	dep := newDeployment("shop", "checkout-api", "250m", "256Mi")
	policy := &rightsizerv1alpha1.ResourcePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "shop"},
		Spec: rightsizerv1alpha1.ResourcePolicySpec{
			Window: "7d", MinSamples: 50, ChangeThresholdPercent: 10,
			CPU: rightsizerv1alpha1.ResourceThresholds{Percentile: 95, LimitMultiplier: "2"},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dep, policy).
		WithStatusSubresource(&rightsizerv1alpha1.ResourcePolicy{}).Build()

	usage := metrics.NewFakeSource()
	usage.SetCPU("shop", "checkout-api", "app", constantSamples(3, 400)) // fewer than MinSamples=50
	usage.SetMemory("shop", "checkout-api", "app", constantSamples(3, 400*1024*1024))

	r := &ResourcePolicyReconciler{Client: c, Scheme: scheme, UsageSource: usage}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(policy)})
	if err != nil {
		t.Fatalf("Reconcile should not error on insufficient samples: %v", err)
	}

	var updated rightsizerv1alpha1.ResourcePolicy
	_ = c.Get(context.Background(), client.ObjectKeyFromObject(policy), &updated)
	if len(updated.Status.Recommendations) != 0 {
		t.Fatalf("expected no recommendations when samples are insufficient, got %+v", updated.Status.Recommendations)
	}
}

func TestReconcile_WorkloadSelectorFiltersDeployments(t *testing.T) {
	scheme := newScheme(t)
	included := newDeployment("shop", "checkout-api", "250m", "256Mi")
	included.Labels = map[string]string{"rightsizer": "on"}
	excluded := newDeployment("shop", "legacy-batch", "250m", "256Mi")

	policy := &rightsizerv1alpha1.ResourcePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "shop"},
		Spec: rightsizerv1alpha1.ResourcePolicySpec{
			Window: "7d", MinSamples: 5, ChangeThresholdPercent: 10,
			CPU:              rightsizerv1alpha1.ResourceThresholds{Percentile: 95, LimitMultiplier: "2"},
			WorkloadSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"rightsizer": "on"}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(included, excluded, policy).
		WithStatusSubresource(&rightsizerv1alpha1.ResourcePolicy{}).Build()

	usage := metrics.NewFakeSource()
	usage.SetCPU("shop", "checkout-api", "app", constantSamples(20, 400))
	usage.SetMemory("shop", "checkout-api", "app", constantSamples(20, 400*1024*1024))
	usage.SetCPU("shop", "legacy-batch", "app", constantSamples(20, 400))
	usage.SetMemory("shop", "legacy-batch", "app", constantSamples(20, 400*1024*1024))

	r := &ResourcePolicyReconciler{Client: c, Scheme: scheme, UsageSource: usage}
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(policy)})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var updated rightsizerv1alpha1.ResourcePolicy
	_ = c.Get(context.Background(), client.ObjectKeyFromObject(policy), &updated)
	if len(updated.Status.Recommendations) != 1 {
		t.Fatalf("expected exactly 1 recommendation (label-selected workload only), got %+v", updated.Status.Recommendations)
	}
	if updated.Status.Recommendations[0].Workload != "checkout-api" {
		t.Fatalf("expected checkout-api recommendation, got %+v", updated.Status.Recommendations[0])
	}
}
