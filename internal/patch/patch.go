// Package patch applies right-sizing recommendations to a Deployment
// manifest's YAML source, in place, without going through a full
// unmarshal/marshal round trip. That keeps comments, key ordering, and
// unrelated formatting in the GitOps repo untouched, which matters a lot
// when the output is going to show up as a human-reviewed diff in a pull
// request.
package patch

import (
	"bytes"
	"fmt"
	"io"

	"gopkg.in/yaml.v3"
)

// ContainerPatch is the new resource values to apply to one container.
// Empty strings leave that specific field untouched.
type ContainerPatch struct {
	Container     string
	CPURequest    string
	CPULimit      string
	MemoryRequest string
	MemoryLimit   string
}

// ErrContainerNotFound is returned when a requested container name does not
// exist in any of the manifest's Deployment documents.
type ErrContainerNotFound struct {
	Container string
}

func (e *ErrContainerNotFound) Error() string {
	return fmt.Sprintf("container %q not found in manifest", e.Container)
}

// ApplyDeploymentPatches rewrites the resources.requests/resources.limits of
// the named containers inside a Deployment YAML manifest, returning the
// patched document bytes. The manifest may be a multi-document YAML stream
// (e.g. a Deployment followed by a Service separated by "---"); every
// document is scanned for matching containers under both
// spec.template.spec.containers and spec.template.spec.initContainers.
//
// If a patch's Container is not found in any document,
// ApplyDeploymentPatches returns ErrContainerNotFound and no output is
// produced (all-or-nothing), so a typo in a controller-generated patch can
// never silently no-op.
func ApplyDeploymentPatches(manifest []byte, patches []ContainerPatch) ([]byte, error) {
	docs, err := decodeAll(manifest)
	if err != nil {
		return nil, fmt.Errorf("parsing manifest yaml: %w", err)
	}

	applied := make(map[string]bool, len(patches))
	byName := make(map[string]ContainerPatch, len(patches))
	for _, p := range patches {
		byName[p.Container] = p
		applied[p.Container] = false
	}

	for _, doc := range docs {
		walkContainers(doc, func(containerNode *yaml.Node) {
			name := mapValue(containerNode, "name")
			p, ok := byName[name]
			if !ok {
				return
			}
			patchContainerResources(containerNode, p)
			applied[name] = true
		})
	}

	for name, ok := range applied {
		if !ok {
			return nil, &ErrContainerNotFound{Container: name}
		}
	}

	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	for _, doc := range docs {
		if err := enc.Encode(doc); err != nil {
			_ = enc.Close()
			return nil, fmt.Errorf("marshaling patched manifest: %w", err)
		}
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("closing yaml encoder: %w", err)
	}
	return buf.Bytes(), nil
}

// decodeAll parses every document in a (possibly multi-document) YAML
// stream into its root yaml.Node, preserving document order.
func decodeAll(manifest []byte) ([]*yaml.Node, error) {
	dec := yaml.NewDecoder(bytes.NewReader(manifest))
	var docs []*yaml.Node
	for {
		var doc yaml.Node
		err := dec.Decode(&doc)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		d := doc
		docs = append(docs, &d)
	}
	return docs, nil
}

// walkContainers finds every mapping node that looks like a container entry
// (i.e. is an element of a `containers:` or `initContainers:` sequence under
// spec.template.spec) and invokes fn on it.
func walkContainers(n *yaml.Node, fn func(*yaml.Node)) {
	if n == nil {
		return
	}
	switch n.Kind {
	case yaml.DocumentNode:
		for _, c := range n.Content {
			walkContainers(c, fn)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			key := n.Content[i]
			val := n.Content[i+1]
			if key.Value == "containers" || key.Value == "initContainers" {
				if val.Kind == yaml.SequenceNode {
					for _, item := range val.Content {
						fn(item)
					}
					continue
				}
			}
			walkContainers(val, fn)
		}
	case yaml.SequenceNode:
		for _, c := range n.Content {
			walkContainers(c, fn)
		}
	}
}

// mapValue returns the string scalar value for key in a mapping node, or ""
// if absent or not a scalar.
func mapValue(mapping *yaml.Node, key string) string {
	if mapping.Kind != yaml.MappingNode {
		return ""
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			v := mapping.Content[i+1]
			if v.Kind == yaml.ScalarNode {
				return v.Value
			}
		}
	}
	return ""
}

// patchContainerResources mutates containerNode's resources.requests/limits
// in place, creating any missing resources/requests/limits mappings.
func patchContainerResources(containerNode *yaml.Node, p ContainerPatch) {
	resources := ensureChildMapping(containerNode, "resources")
	if p.CPURequest != "" || p.MemoryRequest != "" {
		requests := ensureChildMapping(resources, "requests")
		setScalar(requests, "cpu", p.CPURequest)
		setScalar(requests, "memory", p.MemoryRequest)
	}
	if p.CPULimit != "" || p.MemoryLimit != "" {
		limits := ensureChildMapping(resources, "limits")
		setScalar(limits, "cpu", p.CPULimit)
		setScalar(limits, "memory", p.MemoryLimit)
	}
}

// ensureChildMapping returns the mapping node at mapping[key], creating it
// (and appending it to mapping's content) if it does not already exist.
func ensureChildMapping(mapping *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i+1]
		}
	}
	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
	valNode := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	mapping.Content = append(mapping.Content, keyNode, valNode)
	return valNode
}

// setScalar sets mapping[key] = value (as a plain string scalar), skipping
// entirely if value is empty so callers can patch CPU without touching
// memory (or vice versa).
func setScalar(mapping *yaml.Node, key, value string) {
	if value == "" {
		return
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content[i+1].Value = value
			mapping.Content[i+1].Tag = "!!str"
			mapping.Content[i+1].Style = 0
			return
		}
	}
	keyNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: key}
	valNode := &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: value}
	mapping.Content = append(mapping.Content, keyNode, valNode)
}
