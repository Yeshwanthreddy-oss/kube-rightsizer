package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ResourceThresholds defines the percentile, headroom and clamping rules used
// to turn raw usage samples into a recommended request/limit pair for a
// single resource (cpu or memory).
type ResourceThresholds struct {
	// Percentile is the percentile (0-100) of the historical usage
	// distribution used as the base recommendation, e.g. 95 for p95.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +kubebuilder:default=95
	Percentile int32 `json:"percentile,omitempty"`

	// HeadroomPercent is added on top of the percentile value as a safety
	// margin before it becomes the recommended request, e.g. 15 means
	// recommended = p95 * 1.15.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=500
	// +kubebuilder:default=15
	HeadroomPercent int32 `json:"headroomPercent,omitempty"`

	// LimitMultiplier scales the recommended request into a recommended
	// limit, e.g. 2.0 means limit = request * 2.
	// +kubebuilder:validation:Type=string
	// +kubebuilder:default="2"
	LimitMultiplier string `json:"limitMultiplier,omitempty"`

	// MinRequest is a floor below which the recommendation is never
	// lowered, expressed as a Kubernetes quantity string (e.g. "50m",
	// "64Mi").
	MinRequest string `json:"minRequest,omitempty"`

	// MaxRequest is a ceiling above which the recommendation is never
	// raised, expressed as a Kubernetes quantity string.
	MaxRequest string `json:"maxRequest,omitempty"`
}

// ContainerExclusion excludes a specific workload/container pair (or all
// containers of a workload when Container is empty) from recommendations.
type ContainerExclusion struct {
	// Workload is the Deployment name to exclude.
	Workload string `json:"workload"`
	// Container is the container name to exclude. If empty, the whole
	// workload is excluded.
	Container string `json:"container,omitempty"`
}

// GitOpsTarget describes where recommendation patches should be sent as a
// pull request instead of being applied live to the cluster.
type GitOpsTarget struct {
	// Repo is the "owner/name" of the GitHub repository holding the
	// GitOps manifests.
	// +kubebuilder:validation:Required
	Repo string `json:"repo"`

	// BaseBranch is the branch PRs are opened against. Defaults to "main".
	// +kubebuilder:default="main"
	BaseBranch string `json:"baseBranch,omitempty"`

	// ManifestPath is a directory (relative to the repo root) that is
	// searched for the Deployment manifest matching a given workload.
	// +kubebuilder:default="."
	ManifestPath string `json:"manifestPath,omitempty"`

	// PRLabels are labels applied to generated pull requests.
	PRLabels []string `json:"prLabels,omitempty"`
}

// ResourcePolicySpec defines the desired right-sizing behavior for the
// workloads in (or selected from) a namespace.
type ResourcePolicySpec struct {
	// NamespaceSelector restricts which namespaces this policy applies to
	// when the policy itself lives in a central control namespace. If
	// empty, the policy only applies to the namespace it is created in.
	NamespaceSelector *metav1.LabelSelector `json:"namespaceSelector,omitempty"`

	// WorkloadSelector restricts which Deployments are considered. An
	// empty selector matches all Deployments in scope.
	WorkloadSelector *metav1.LabelSelector `json:"workloadSelector,omitempty"`

	// Window is the rolling look-back window of historical usage pulled
	// from Prometheus, e.g. "7d". Parsed with time.ParseDuration-style
	// units plus "d" for days.
	// +kubebuilder:default="7d"
	Window string `json:"window,omitempty"`

	// MinSamples is the minimum number of usage samples required before a
	// recommendation is emitted for a container. Protects against
	// recommending off of too little history.
	// +kubebuilder:default=10
	MinSamples int32 `json:"minSamples,omitempty"`

	// CPU thresholds for the recommendation engine.
	CPU ResourceThresholds `json:"cpu,omitempty"`

	// Memory thresholds for the recommendation engine.
	Memory ResourceThresholds `json:"memory,omitempty"`

	// ChangeThresholdPercent is the minimum relative change between the
	// current request and the recommended request before a PR is opened.
	// Prevents PR noise from marginal drift.
	// +kubebuilder:default=10
	ChangeThresholdPercent int32 `json:"changeThresholdPercent,omitempty"`

	// Exclusions lists workload/container pairs to skip entirely.
	Exclusions []ContainerExclusion `json:"exclusions,omitempty"`

	// GitOps describes where recommendations are published as pull
	// requests. If nil, the controller only updates Status with
	// recommendations and emits events (dry-run mode).
	GitOps *GitOpsTarget `json:"gitOps,omitempty"`

	// Suspend pauses reconciliation for this policy without deleting it.
	Suspend bool `json:"suspend,omitempty"`
}

// ContainerRecommendation is the computed recommendation for a single
// container within a workload.
type ContainerRecommendation struct {
	Workload  string `json:"workload"`
	Container string `json:"container"`

	CurrentCPURequest string `json:"currentCPURequest,omitempty"`
	CurrentMemRequest string `json:"currentMemRequest,omitempty"`

	RecommendedCPURequest string `json:"recommendedCPURequest,omitempty"`
	RecommendedCPULimit   string `json:"recommendedCPULimit,omitempty"`
	RecommendedMemRequest string `json:"recommendedMemRequest,omitempty"`
	RecommendedMemLimit   string `json:"recommendedMemLimit,omitempty"`

	SampleCount int32 `json:"sampleCount"`
	// ChangePercent is the largest of the CPU/Memory relative request
	// changes, used for threshold + reporting.
	ChangePercent int32 `json:"changePercent"`
}

// ResourcePolicyStatus reflects the last observed reconciliation outcome.
type ResourcePolicyStatus struct {
	// ObservedGeneration is the .metadata.generation last reconciled.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// LastReconcileTime is when the controller last finished evaluating
	// this policy.
	LastReconcileTime *metav1.Time `json:"lastReconcileTime,omitempty"`

	// Recommendations holds the most recently computed per-container
	// recommendations, regardless of whether a PR was opened for them.
	Recommendations []ContainerRecommendation `json:"recommendations,omitempty"`

	// LastPullRequestURL is the URL of the most recently opened GitOps
	// pull request, if any.
	LastPullRequestURL string `json:"lastPullRequestURL,omitempty"`

	// Conditions represent the latest available observations of the
	// policy's state.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=rpol
// +kubebuilder:printcolumn:name="Window",type=string,JSONPath=`.spec.window`
// +kubebuilder:printcolumn:name="Suspend",type=boolean,JSONPath=`.spec.suspend`
// +kubebuilder:printcolumn:name="Last Reconcile",type=string,JSONPath=`.status.lastReconcileTime`

// ResourcePolicy configures continuous, safe resource right-sizing for the
// Deployments it selects: how much historical usage to look at, how much
// safety headroom to keep, and where to send the resulting GitOps pull
// request.
type ResourcePolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ResourcePolicySpec   `json:"spec,omitempty"`
	Status ResourcePolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ResourcePolicyList contains a list of ResourcePolicy.
type ResourcePolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ResourcePolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ResourcePolicy{}, &ResourcePolicyList{})
}
