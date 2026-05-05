//go:build envtest

// This file is only compiled with `-tags=envtest`, and is never part of the
// default `go build ./... && go test ./...` path so the project's main test
// command never needs a downloaded kube-apiserver/etcd or network access.
// It gives an extra layer of confidence beyond the fake-client unit tests
// in resourcepolicy_controller_test.go by reconciling against a real (if
// ephemeral) Kubernetes control plane.
//
// Run locally with:
//
//	go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
//	export KUBEBUILDER_ASSETS=$(setup-envtest use 1.31.0 -p path)
//	go test ./internal/controller/... -tags=envtest -run TestEnvtest -v
package controller

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	rightsizerv1alpha1 "github.com/kushagrasikka/kube-rightsizer/api/v1alpha1"
	"github.com/kushagrasikka/kube-rightsizer/internal/metrics"
)

func TestEnvtest_ReconcileAgainstRealAPIServer(t *testing.T) {
	testEnv := &envtest.Environment{
		CRDDirectoryPaths:     []string{"../../config/crd/bases"},
		ErrorIfCRDPathMissing: true,
	}

	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatalf("starting envtest environment: %v", err)
	}
	defer func() {
		if err := testEnv.Stop(); err != nil {
			t.Errorf("stopping envtest environment: %v", err)
		}
	}()

	scheme := newScheme(t)
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("building client: %v", err)
	}

	ctx := context.Background()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "shop-envtest"}}
	if err := c.Create(ctx, ns); err != nil {
		t.Fatalf("creating namespace: %v", err)
	}

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "checkout-api", Namespace: ns.Name},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "checkout-api"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "checkout-api"}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{
						Name:  "app",
						Image: "nginx:1.27-alpine",
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("250m"),
								corev1.ResourceMemory: resource.MustParse("256Mi"),
							},
						},
					}},
				},
			},
		},
	}
	if err := c.Create(ctx, dep); err != nil {
		t.Fatalf("creating deployment: %v", err)
	}

	policy := &rightsizerv1alpha1.ResourcePolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: ns.Name},
		Spec: rightsizerv1alpha1.ResourcePolicySpec{
			Window: "7d", MinSamples: 5, ChangeThresholdPercent: 10,
			CPU:    rightsizerv1alpha1.ResourceThresholds{Percentile: 95, HeadroomPercent: 15, LimitMultiplier: "2"},
			Memory: rightsizerv1alpha1.ResourceThresholds{Percentile: 95, HeadroomPercent: 15, LimitMultiplier: "2"},
		},
	}
	if err := c.Create(ctx, policy); err != nil {
		t.Fatalf("creating policy: %v", err)
	}

	usage := metrics.NewFakeSource()
	samples := make([]float64, 20)
	for i := range samples {
		samples[i] = 400
	}
	usage.SetCPU(ns.Name, "checkout-api", "app", samples)
	memSamples := make([]float64, 20)
	for i := range memSamples {
		memSamples[i] = 300 * 1024 * 1024
	}
	usage.SetMemory(ns.Name, "checkout-api", "app", memSamples)

	r := &ResourcePolicyReconciler{Client: c, Scheme: scheme, UsageSource: usage, Clock: time.Now}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(policy)}); err != nil {
		t.Fatalf("Reconcile against real API server: %v", err)
	}

	var updated rightsizerv1alpha1.ResourcePolicy
	if err := c.Get(ctx, client.ObjectKeyFromObject(policy), &updated); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(updated.Status.Recommendations) != 1 {
		t.Fatalf("expected 1 recommendation from real API server round-trip, got %+v", updated.Status.Recommendations)
	}
}
