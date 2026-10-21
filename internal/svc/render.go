package svc

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Platform constants the renderer owns. A spec can change none of them.
const (
	ackNamespace     = "ack-services"      // where ACK custom resources for service roles live
	infraProject     = "services-infra"    // platform Argo CD project for rendered infra
	argoNamespace    = "argocd"            // where Application objects live
	secretStoreName  = "ssm"               // per-namespace SecretStore; its role reads /<env>/<team>/* only
	podIdentityAgent = "169.254.170.23/32" // EKS Pod Identity agent, link-local on every node
	gitopsRepo       = "https://git.example.org/platform-gitops.git"
	nameLabel        = "app.kubernetes.io/name"
)

// Object is one rendered file.
type Object struct {
	Name string // file name, e.g. networkpolicy.yaml
	Doc  map[string]any
}

// YAML encodes the object deterministically: maps are written with sorted
// keys, and nothing depends on time or randomness.
func (o Object) YAML() ([]byte, error) {
	var b bytes.Buffer
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(o.Doc); err != nil {
		return nil, err
	}
	return b.Bytes(), enc.Close()
}

type m = map[string]any

// Render produces the objects for one environment, leaving out every item in
// refused. It assumes Check found no spec-level error.
func Render(s *Spec, p *Platform, env string, refused map[string]bool) ([]Object, []string) {
	e := p.Envs[env]
	cfg := e.Config
	t := team(s, p)
	ns := cfg.Teams[t].Namespace
	var notes []string

	meta := func(name, namespace string) m {
		md := m{"name": name, "labels": m{
			nameLabel:                      s.Service,
			"app.kubernetes.io/managed-by": "svcrender",
			"platform.example.org/team":    t,
		}, "annotations": m{"platform.example.org/spec-digest": s.Digest}}
		if namespace != "" {
			md["namespace"] = namespace
		}
		return md
	}

	var objs []Object
	objs = append(objs, Object{"serviceaccount.yaml", m{
		"apiVersion": "v1", "kind": "ServiceAccount", "metadata": meta(s.Service, ns),
	}})

	statements := policyStatements(s, e, refused)
	if len(statements) > 0 {
		roleName := fmt.Sprintf("svc-%s-%s", t, s.Service)
		trust := jsonString(m{"Version": "2012-10-17", "Statement": []m{{
			"Effect":    "Allow",
			"Principal": m{"Service": "pods.eks.amazonaws.com"},
			"Action":    []string{"sts:AssumeRole", "sts:TagSession"},
			"Condition": m{"StringEquals": m{
				"aws:SourceAccount":                         cfg.AWSAccountID,
				"aws:RequestTag/eks-cluster-name":           cfg.ClusterName,
				"aws:RequestTag/kubernetes-namespace":       ns,
				"aws:RequestTag/kubernetes-service-account": s.Service,
			}},
		}}})
		objs = append(objs,
			Object{"iam-role.yaml", m{
				"apiVersion": "iam.services.k8s.aws/v1alpha1", "kind": "Role", "metadata": meta(roleName, ackNamespace),
				"spec": m{
					"name":                     roleName,
					"description":              fmt.Sprintf("%s (%s), rendered from its service spec", s.Service, t),
					"permissionsBoundary":      cfg.PermissionsBoundaryARN,
					"assumeRolePolicyDocument": trust,
					"inlinePolicies":           m{"service": jsonString(m{"Version": "2012-10-17", "Statement": statements})},
					"tags":                     []m{{"key": "team", "value": t}, {"key": "service", "value": s.Service}},
				},
			}},
			Object{"pod-identity-association.yaml", m{
				"apiVersion": "eks.services.k8s.aws/v1alpha1", "kind": "PodIdentityAssociation", "metadata": meta(s.Service, ackNamespace),
				"spec": m{
					"clusterName":    cfg.ClusterName,
					"namespace":      ns,
					"serviceAccount": s.Service,
					"roleARN":        fmt.Sprintf("arn:aws:iam::%s:role/%s", cfg.AWSAccountID, roleName),
				},
			}})
	}

	objs = append(objs, Object{"networkpolicy.yaml", m{
		"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy", "metadata": meta(s.Service, ns),
		"spec": m{
			"podSelector": m{"matchLabels": m{nameLabel: s.Service}},
			"policyTypes": []string{"Ingress", "Egress"},
			"ingress":     ingressRules(s, cfg, refused),
			"egress":      egressRules(s, cfg, refused, len(statements) > 0),
		},
	}})

	var data []m
	for i, sec := range s.Secrets {
		if refused[fmt.Sprintf("secrets[%d]", i)] {
			continue
		}
		data = append(data, m{"secretKey": sec.Key, "remoteRef": m{"key": cfg.SSMPrefix + "/" + strings.TrimPrefix(sec.SSM, "/")}})
	}
	if len(data) > 0 {
		objs = append(objs, Object{"externalsecret.yaml", m{
			"apiVersion": "external-secrets.io/v1", "kind": "ExternalSecret", "metadata": meta(s.Service, ns),
			"spec": m{
				"refreshInterval": "1h",
				"secretStoreRef":  m{"kind": "SecretStore", "name": secretStoreName},
				"target":          m{"name": s.Service, "creationPolicy": "Owner", "deletionPolicy": "Retain"},
				"data":            data,
			},
		}})
	}

	sync := m{}
	if cfg.ArgoCD.AutoSync {
		sync["automated"] = m{"prune": true, "selfHeal": true}
	}
	objs = append(objs, Object{"application-infra.yaml", m{
		"apiVersion": "argoproj.io/v1alpha1", "kind": "Application", "metadata": meta(s.Service+"-infra", argoNamespace),
		"spec": m{
			"project":     infraProject,
			"destination": m{"server": "https://kubernetes.default.svc", "namespace": ns},
			"source":      m{"repoURL": gitopsRepo, "targetRevision": "main", "path": fmt.Sprintf("rendered/%s/%s", env, s.Service)},
			"syncPolicy":  sync,
		},
	}})

	if ptr := e.Pointers[s.Service]; ptr != nil {
		objs = append(objs, Object{"application.yaml", m{
			"apiVersion": "argoproj.io/v1alpha1", "kind": "Application", "metadata": meta(s.Service, argoNamespace),
			"spec": m{
				"project":     cfg.Teams[t].Project,
				"destination": m{"server": "https://kubernetes.default.svc", "namespace": ns},
				"source": m{
					"repoURL":        strings.TrimPrefix(cfg.ChartRegistry, "oci://"),
					"chart":          s.Chart,
					"targetRevision": ptr.ChartVersion,
					"helm": m{"valuesObject": m{
						"image":          m{"repository": cfg.ImageRegistry + "/" + s.Image, "tag": ptr.ImageTag},
						"serviceAccount": m{"create": false, "name": s.Service},
					}},
				},
				"syncPolicy": sync,
			},
		}})
	} else {
		notes = append(notes, fmt.Sprintf("no chart Application yet: Kargo has not promoted %s to %s", s.Service, env))
	}
	slices.SortFunc(objs, func(a, b Object) int { return strings.Compare(a.Name, b.Name) })
	return objs, notes
}

// RenderECR renders the image repository in the dev account. It does not
// depend on an environment: stg and prd pull through a cache of dev, by a
// platform-wide rule for the trading/ prefix, not one rule per service.
func RenderECR(s *Spec) Object {
	return Object{"ecr-repository.yaml", m{
		"apiVersion": "ecr.services.k8s.aws/v1alpha1", "kind": "Repository",
		"metadata": m{"name": strings.ReplaceAll(s.Image, "/", "-"), "namespace": ackNamespace,
			"labels": m{nameLabel: s.Service, "app.kubernetes.io/managed-by": "svcrender"}},
		"spec": m{
			"name":                       s.Image,
			"imageTagMutability":         "IMMUTABLE",
			"imageScanningConfiguration": m{"scanOnPush": true},
		},
	}}
}

type statement struct {
	Sid      string
	Effect   string
	Action   []string
	Resource []string
}

func policyStatements(s *Spec, e *Env, refused map[string]bool) []statement {
	if refused["permissions"] {
		return nil
	}
	var out []statement
	for i, perm := range s.Permissions {
		if refused[fmt.Sprintf("permissions[%d]", i)] {
			continue
		}
		st := statement{Sid: fmt.Sprintf("Spec%d", i), Effect: "Allow", Action: perm.Actions}
		for _, r := range perm.Resources {
			rt, name, _ := splitRef(r)
			arn, _ := rt.arn(expandEnv(name, e.Config.Env), e) // CheckEnv refused anything that fails here
			st.Resource = append(st.Resource, arn)
		}
		out = append(out, st)
	}
	return out
}

func ports(ps []int) []m {
	var out []m
	for _, p := range ps {
		out = append(out, m{"port": p, "protocol": "TCP"})
	}
	return out
}

func namespacePeer(ns string) m {
	return m{"namespaceSelector": m{"matchLabels": m{"kubernetes.io/metadata.name": ns}}}
}

func ingressRules(s *Spec, cfg EnvConfig, refused map[string]bool) []m {
	out := []m{}
	for i, r := range s.Ingress {
		if refused[fmt.Sprintf("ingress[%d]", i)] {
			continue
		}
		from := namespacePeer(r.Namespace)
		if r.CIDR == vpcToken {
			from = m{"ipBlock": m{"cidr": cfg.VPCCIDR}}
		}
		out = append(out, m{"from": []m{from}, "ports": ports(r.Ports)})
	}
	return out
}

func egressRules(s *Spec, cfg EnvConfig, refused map[string]bool, hasRole bool) []m {
	// Platform baseline: every pod resolves names, and a pod with a role
	// fetches its credentials from the node-local Pod Identity agent.
	out := []m{{
		"to":    []m{{"namespaceSelector": m{"matchLabels": m{"kubernetes.io/metadata.name": "kube-system"}}, "podSelector": m{"matchLabels": m{"k8s-app": "kube-dns"}}}},
		"ports": []m{{"port": 53, "protocol": "UDP"}, {"port": 53, "protocol": "TCP"}},
	}}
	if hasRole {
		out = append(out, m{"to": []m{{"ipBlock": m{"cidr": podIdentityAgent}}}, "ports": ports([]int{80})})
	}
	for i, r := range s.Egress {
		if refused[fmt.Sprintf("egress[%d]", i)] {
			continue
		}
		var to m
		switch r.CIDR {
		case vpcToken:
			to = m{"ipBlock": m{"cidr": cfg.VPCCIDR}}
		case "0.0.0.0/0":
			// The internet, not the VPC: in-VPC traffic must be declared on its own.
			to = m{"ipBlock": m{"cidr": "0.0.0.0/0", "except": []string{cfg.VPCCIDR}}}
		default:
			to = namespacePeer(r.Namespace)
		}
		out = append(out, m{"to": []m{to}, "ports": ports(r.Ports)})
	}
	return out
}

// jsonString indents policy documents so a permission change is a readable
// line diff in review.
func jsonString(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		panic(err) // only static shapes are marshalled
	}
	return string(b)
}
