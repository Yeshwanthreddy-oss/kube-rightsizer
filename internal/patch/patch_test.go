package patch

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const simpleDeployment = `apiVersion: apps/v1
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
        - name: sidecar-proxy
          image: ghcr.io/shop/proxy:2.0.0
          resources:
            requests:
              cpu: 50m
              memory: 64Mi
`

func mustPatch(t *testing.T, manifest string, patches []ContainerPatch) string {
	t.Helper()
	out, err := ApplyDeploymentPatches([]byte(manifest), patches)
	if err != nil {
		t.Fatalf("ApplyDeploymentPatches: %v", err)
	}
	return string(out)
}

// parseDeploymentContainerResources round-trips the patched YAML through a
// generic map to make assertions independent of formatting/indentation.
type resourceList struct {
	CPU    string `yaml:"cpu"`
	Memory string `yaml:"memory"`
}
type resources struct {
	Requests resourceList `yaml:"requests"`
	Limits   resourceList `yaml:"limits"`
}
type container struct {
	Name      string    `yaml:"name"`
	Resources resources `yaml:"resources"`
}
type deployment struct {
	Spec struct {
		Template struct {
			Spec struct {
				Containers     []container `yaml:"containers"`
				InitContainers []container `yaml:"initContainers"`
			} `yaml:"spec"`
		} `yaml:"template"`
	} `yaml:"spec"`
}

func parseDeployment(t *testing.T, out string) deployment {
	t.Helper()
	var d deployment
	// Only decode the first document; tests that need multiple documents
	// split on "---" themselves.
	dec := yaml.NewDecoder(strings.NewReader(out))
	if err := dec.Decode(&d); err != nil {
		t.Fatalf("decoding patched output: %v\n---\n%s", err, out)
	}
	return d
}

func findContainer(d deployment, name string) *container {
	for i := range d.Spec.Template.Spec.Containers {
		if d.Spec.Template.Spec.Containers[i].Name == name {
			return &d.Spec.Template.Spec.Containers[i]
		}
	}
	return nil
}

func TestApplyDeploymentPatches_UpdatesRequestsAndLimits(t *testing.T) {
	out := mustPatch(t, simpleDeployment, []ContainerPatch{
		{Container: "app", CPURequest: "300m", CPULimit: "600m", MemoryRequest: "300Mi", MemoryLimit: "600Mi"},
	})
	d := parseDeployment(t, out)
	app := findContainer(d, "app")
	if app == nil {
		t.Fatal("container 'app' not found in patched output")
	}
	if app.Resources.Requests.CPU != "300m" || app.Resources.Requests.Memory != "300Mi" {
		t.Fatalf("requests not patched: %+v", app.Resources.Requests)
	}
	if app.Resources.Limits.CPU != "600m" || app.Resources.Limits.Memory != "600Mi" {
		t.Fatalf("limits not patched: %+v", app.Resources.Limits)
	}
}

func TestApplyDeploymentPatches_LeavesOtherContainersUntouched(t *testing.T) {
	out := mustPatch(t, simpleDeployment, []ContainerPatch{
		{Container: "app", CPURequest: "300m"},
	})
	d := parseDeployment(t, out)
	sidecar := findContainer(d, "sidecar-proxy")
	if sidecar == nil {
		t.Fatal("sidecar-proxy missing after patch")
	}
	if sidecar.Resources.Requests.CPU != "50m" || sidecar.Resources.Requests.Memory != "64Mi" {
		t.Fatalf("sidecar resources changed unexpectedly: %+v", sidecar.Resources.Requests)
	}
}

func TestApplyDeploymentPatches_PartialPatchLeavesMemoryUntouched(t *testing.T) {
	out := mustPatch(t, simpleDeployment, []ContainerPatch{
		{Container: "app", CPURequest: "400m"}, // no memory fields set
	})
	d := parseDeployment(t, out)
	app := findContainer(d, "app")
	if app.Resources.Requests.CPU != "400m" {
		t.Fatalf("CPU request not patched: %+v", app.Resources.Requests)
	}
	if app.Resources.Requests.Memory != "256Mi" {
		t.Fatalf("memory request should be untouched, got %q", app.Resources.Requests.Memory)
	}
	if app.Resources.Limits.CPU != "500m" {
		t.Fatalf("CPU limit should be untouched since patch didn't set it, got %q", app.Resources.Limits.CPU)
	}
}

func TestApplyDeploymentPatches_CreatesMissingResourcesBlock(t *testing.T) {
	const noResources = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: bare
spec:
  template:
    spec:
      containers:
        - name: app
          image: ghcr.io/shop/bare:1.0.0
`
	out := mustPatch(t, noResources, []ContainerPatch{
		{Container: "app", CPURequest: "100m", MemoryRequest: "128Mi"},
	})
	d := parseDeployment(t, out)
	app := findContainer(d, "app")
	if app == nil {
		t.Fatal("container not found")
	}
	if app.Resources.Requests.CPU != "100m" || app.Resources.Requests.Memory != "128Mi" {
		t.Fatalf("resources block not created correctly: %+v", app.Resources.Requests)
	}
}

func TestApplyDeploymentPatches_InitContainerSupported(t *testing.T) {
	const withInit = `apiVersion: apps/v1
kind: Deployment
metadata:
  name: with-init
spec:
  template:
    spec:
      initContainers:
        - name: migrate
          image: ghcr.io/shop/migrate:1.0.0
          resources:
            requests:
              cpu: 100m
              memory: 128Mi
      containers:
        - name: app
          image: ghcr.io/shop/app:1.0.0
`
	out := mustPatch(t, withInit, []ContainerPatch{
		{Container: "migrate", CPURequest: "200m"},
	})
	d := parseDeployment(t, out)
	if len(d.Spec.Template.Spec.InitContainers) != 1 {
		t.Fatalf("expected 1 init container, got %d", len(d.Spec.Template.Spec.InitContainers))
	}
	if d.Spec.Template.Spec.InitContainers[0].Resources.Requests.CPU != "200m" {
		t.Fatalf("init container not patched: %+v", d.Spec.Template.Spec.InitContainers[0].Resources.Requests)
	}
}

func TestApplyDeploymentPatches_MultiDocumentStreamOnlyPatchesDeployment(t *testing.T) {
	const multiDoc = simpleDeployment + `---
apiVersion: v1
kind: Service
metadata:
  name: checkout-api
spec:
  selector:
    app: checkout-api
  ports:
    - port: 80
`
	out, err := ApplyDeploymentPatches([]byte(multiDoc), []ContainerPatch{
		{Container: "app", CPURequest: "999m"},
	})
	if err != nil {
		t.Fatalf("ApplyDeploymentPatches: %v", err)
	}
	docs := strings.Split(string(out), "---")
	if len(docs) != 2 {
		t.Fatalf("expected 2 documents in output, got %d:\n%s", len(docs), out)
	}
	if !strings.Contains(docs[0], "999m") {
		t.Fatalf("expected patched cpu request in first document:\n%s", docs[0])
	}
	if !strings.Contains(docs[1], "kind: Service") {
		t.Fatalf("expected Service document preserved:\n%s", docs[1])
	}
	if strings.Contains(docs[1], "999m") {
		t.Fatalf("Service document should not have been touched:\n%s", docs[1])
	}
}

func TestApplyDeploymentPatches_UnknownContainerReturnsTypedError(t *testing.T) {
	_, err := ApplyDeploymentPatches([]byte(simpleDeployment), []ContainerPatch{
		{Container: "does-not-exist", CPURequest: "1"},
	})
	if err == nil {
		t.Fatal("expected error for unknown container")
	}
	notFound, ok := err.(*ErrContainerNotFound)
	if !ok {
		t.Fatalf("expected *ErrContainerNotFound, got %T: %v", err, err)
	}
	if notFound.Container != "does-not-exist" {
		t.Fatalf("wrong container in error: %+v", notFound)
	}
}

func TestApplyDeploymentPatches_PreservesUnrelatedFields(t *testing.T) {
	out := mustPatch(t, simpleDeployment, []ContainerPatch{
		{Container: "app", CPURequest: "300m"},
	})
	if !strings.Contains(out, "replicas: 3") {
		t.Fatalf("expected replicas: 3 preserved, got:\n%s", out)
	}
	if !strings.Contains(out, "ghcr.io/shop/checkout-api:1.4.2") {
		t.Fatalf("expected image tag preserved, got:\n%s", out)
	}
	if !strings.Contains(out, "name: checkout-api") {
		t.Fatalf("expected metadata.name preserved, got:\n%s", out)
	}
}

func TestApplyDeploymentPatches_InvalidYAMLReturnsError(t *testing.T) {
	_, err := ApplyDeploymentPatches([]byte("not: [valid: yaml"), []ContainerPatch{{Container: "app"}})
	if err == nil {
		t.Fatal("expected parse error for invalid yaml")
	}
}
