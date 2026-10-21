// Package svc turns a service spec (untrusted, from a service repository) into
// the objects the platform applies, enforcing the platform rules on the way.
//
//	LoadSpec  -> Spec          (schema only: shape, not policy)
//	Check     -> []Finding     (one function per rule, in rules.go)
//	Render    -> []Object      (only what the findings did not refuse)
//	Evaluate  -> Result        (the three together, with a verdict)
package svc

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"sort"

	"gopkg.in/yaml.v3"
)

// Spec is what a service team writes, at platform/service.yaml in its repo.
// Everything environment-specific is either a token ({env}, {vpcCidr}) or a
// name the platform catalog resolves; nothing in it is trusted.
type Spec struct {
	Service      string   `yaml:"service"`
	Team         string   `yaml:"team"`
	Environments []string `yaml:"environments"`
	Image        string   `yaml:"image"`
	Chart        string   `yaml:"chart"`

	Ingress             []Peer       `yaml:"ingress"`
	Egress              []Peer       `yaml:"egress"`
	Permissions         []Permission `yaml:"permissions"`
	Secrets             []Secret     `yaml:"secrets"`
	AcknowledgeRemovals []Removal    `yaml:"acknowledgeRemovals"`

	Unknown map[string]yaml.Node `yaml:",inline"`

	// Path is the file the spec was read from; Raw is its parsed tree, used to
	// scan every value for R5 regardless of which field it sits in.
	Path   string    `yaml:"-"`
	Raw    yaml.Node `yaml:"-"`
	Digest string    `yaml:"-"` // sha256 of the file, stamped on what it renders
}

// Peer is one side of a network rule: a namespace, or a CIDR token.
type Peer struct {
	Namespace string               `yaml:"namespace"`
	CIDR      string               `yaml:"cidr"`
	Ports     []int                `yaml:"ports"`
	Reason    string               `yaml:"reason"`
	Unknown   map[string]yaml.Node `yaml:",inline"`
}

// Permission is one IAM statement, with resources written as platform
// references (s3-object:bucket/key, msk-topic:cluster/topic, ...), never ARNs.
type Permission struct {
	Actions   []string             `yaml:"actions"`
	Resources []string             `yaml:"resources"`
	Unknown   map[string]yaml.Node `yaml:",inline"`
}

// Secret maps one SSM parameter, named without its env prefix, to a key in the
// service's Kubernetes Secret.
type Secret struct {
	Key     string               `yaml:"key"`
	SSM     string               `yaml:"ssm"`
	Unknown map[string]yaml.Node `yaml:",inline"`
}

// Removal is the engineer confirming that a permission the running pod uses
// may go away (R8).
type Removal struct {
	Action   string `yaml:"action"`
	Resource string `yaml:"resource"`
	Reason   string `yaml:"reason"`
}

// LoadSpec parses a spec file. It fails only when the file is not YAML;
// everything else is a Finding, so the engineer gets every problem at once.
func LoadSpec(path string) (*Spec, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseSpec(path, b)
}

// ParseSpec parses spec content; path is used in messages only.
func ParseSpec(path string, b []byte) (*Spec, error) {
	var s Spec
	if err := yaml.Unmarshal(b, &s.Raw); err != nil {
		return nil, fmt.Errorf("%s: not valid YAML: %w", path, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("%s: does not match the spec schema: %w", path, err)
	}
	s.Path = path
	s.Digest = fmt.Sprintf("sha256:%x", sha256.Sum256(b))
	return &s, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
