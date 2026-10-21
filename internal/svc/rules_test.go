package svc

import (
	"strings"
	"testing"
)

func platform(t *testing.T) *Platform {
	t.Helper()
	p, err := LoadPlatform("../../inputs", "../../platform", "stg", "prd")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// base is a valid spec; each test changes one thing about it. permissions
// comes last so a test can append a statement to it.
const base = `
service: odds-ingest
team: ingestion
environments: [stg, prd]
ingress:
  - {namespace: monitoring, ports: [8080]}
egress:
  - {cidr: "{vpcCidr}", ports: [9098]}
  - {cidr: 0.0.0.0/0, ports: [443], reason: vendor feed}
secrets:
  - {key: vendor-api-key, ssm: /ingestion/odds-ingest/vendor-api-key}
permissions:
  - actions: [s3:PutObject]
    resources: ["s3-object:trading-platform-{env}-odds-snapshots/*"]
`

// spec parses yaml as if read from the repository registered for the service
// it names; specFrom says which repository it came from.
func spec(t *testing.T, yaml string) *Spec {
	t.Helper()
	s := specFrom(t, "", yaml)
	s.Repo = "example-org/" + s.Service
	return s
}

func specFrom(t *testing.T, repo, yaml string) *Spec {
	t.Helper()
	s, err := ParseSpec("test.yaml", repo, []byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// with replaces one line of base.
func with(old, new string) string {
	if !strings.Contains(base, old) {
		panic("base has no " + old)
	}
	return strings.Replace(base, old, new, 1)
}

// errorsFor runs every check in both environments and returns the errors of one rule.
func errorsFor(t *testing.T, p *Platform, s *Spec, rule string) []Finding {
	t.Helper()
	var out []Finding
	for _, env := range p.EnvOrder {
		for _, f := range Evaluate(s, p, env).Findings {
			if f.Rule == rule && f.Severity == Error {
				out = append(out, f)
			}
		}
	}
	return out
}

func expectError(t *testing.T, p *Platform, yaml, rule, path, fixContains string) {
	t.Helper()
	fs := errorsFor(t, p, spec(t, yaml), rule)
	for _, f := range fs {
		if f.Path == path {
			if !strings.Contains(f.Fix, fixContains) {
				t.Errorf("%s at %s: fix %q does not say %q", rule, path, f.Fix, fixContains)
			}
			return
		}
	}
	t.Errorf("want %s error at %s; got %v", rule, path, fs)
}

func TestBaseSpecIsAccepted(t *testing.T) {
	p := platform(t)
	for _, env := range p.EnvOrder {
		r := Evaluate(spec(t, base), p, env)
		if r.Verdict != Accept || len(r.Findings) != 0 {
			t.Fatalf("%s: %s %v", env, r.Verdict, r.Findings)
		}
	}
}

func TestR1(t *testing.T) {
	p := platform(t)
	t.Run("spec cannot choose its project", func(t *testing.T) {
		expectError(t, p, base+"project: default\n", "R1", "project", "use a Role and RoleBinding")
	})
	t.Run("spec cannot choose its namespace", func(t *testing.T) {
		expectError(t, p, base+"namespace: state\n", "R1", "namespace", "deploys into namespace ingestion")
	})
	t.Run("team comes from the registry, not the spec", func(t *testing.T) {
		expectError(t, p, with("team: ingestion", "team: state"), "R1", "team", "set team: ingestion")
	})
	t.Run("unregistered repository", func(t *testing.T) {
		expectError(t, p, with("service: odds-ingest", "service: shadow"), "R1", "service", "register the service")
	})
	t.Run("a repository cannot claim another service", func(t *testing.T) {
		// rfq-router's repo writes a spec claiming to be settlement-writer, to
		// get its namespace, role and service account.
		impostor := strings.NewReplacer("odds-ingest", "settlement-writer", "team: ingestion", "team: state").Replace(base)
		s := specFrom(t, "example-org/rfq-router", impostor)
		fs := errorsFor(t, p, s, "R1")
		if len(fs) == 0 || !strings.Contains(fs[0].Problem, "registered as service rfq-router") {
			t.Fatalf("got %v", fs)
		}
		if r := Evaluate(s, p, "stg"); r.Verdict != Refuse || len(r.Objects) != 0 {
			t.Fatalf("verdict %s, %d objects", r.Verdict, len(r.Objects))
		}
	})
	t.Run("chart and image come from the registry only", func(t *testing.T) {
		expectError(t, p, base+"image: trading/settlement-writer\n", "R1", "image", "services.yaml")
	})
	t.Run("a spec-level error refuses everything", func(t *testing.T) {
		r := Evaluate(spec(t, base+"project: default\n"), p, "stg")
		if r.Verdict != Refuse || len(r.Objects) != 0 {
			t.Fatalf("verdict %s with %d objects", r.Verdict, len(r.Objects))
		}
	})
}

func TestR2(t *testing.T) {
	p := platform(t)
	cases := []struct{ name, action, resource, path, fix string }{
		{"service-wide wildcard", "s3:*", "s3-bucket:b", "permissions[0].actions[0]", "s3:Get*, is allowed"},
		{"every action", "\"*\"", "s3-bucket:b", "permissions[0].actions[0]", "list the actions"},
		{"leading wildcard", "s3:*Object", "s3-bucket:b", "permissions[0].actions[0]", "list the actions"},
		{"IAM", "iam:PassRole", "s3-bucket:b", "permissions[0].actions[0]", "never needs to manage IAM"},
		{"STS", "sts:AssumeRole", "s3-bucket:b", "permissions[0].actions[0]", "Cross-account access is not self-service"},
		{"KMS key management", "kms:CreateGrant", "kms-key:ui-assets", "permissions[0].actions[0]", "kms:Decrypt"},
		{"KMS wildcard reaching management", "kms:Create*", "kms-key:ui-assets", "permissions[0].actions[0]", "kms:Decrypt"},
		{"resource star", "s3:GetObject", "\"*\"", "permissions[0].resources[0]", "name each resource"},
		{"wildcard bucket name", "s3:GetObject", "s3-object:trading-*/x", "permissions[0].resources[0]", "wildcards are allowed only after the first /"},
		{"unknown reference type", "s3:GetObject", "bucket:b", "permissions[0].resources[0]", "s3-bucket:<bucket>"},
		{"action and resource of different services", "kafka-cluster:Connect", "s3-bucket:b", "permissions[0].actions[0]", "that service's resources"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			y := with("  - actions: [s3:PutObject]\n    resources: [\"s3-object:trading-platform-{env}-odds-snapshots/*\"]",
				"  - actions: ["+c.action+"]\n    resources: ["+c.resource+"]")
			expectError(t, p, y, "R2", c.path, c.fix)
		})
	}
	t.Run("a family wildcard and KMS use are allowed", func(t *testing.T) {
		y := with("  - actions: [s3:PutObject]", "  - actions: [s3:Get*]") +
			"  - actions: [kms:Decrypt, kms:GenerateDataKey*]\n    resources: [kms-key:ui-assets]\n"
		if fs := errorsFor(t, p, spec(t, y), "R2"); len(fs) != 0 {
			t.Fatal(fs)
		}
	})
	t.Run("a refused permission is left out, the rest renders", func(t *testing.T) {
		y := base + "  - actions: [\"s3:*\"]\n    resources: [s3-bucket:b]\n"
		r := Evaluate(spec(t, y), p, "stg")
		if r.Verdict != Partial {
			t.Fatalf("verdict %s", r.Verdict)
		}
		role := object(t, r, "iam-role.yaml")
		if strings.Contains(role, `"s3:*"`) || !strings.Contains(role, "s3:PutObject") {
			t.Fatalf("role should keep the valid statement only:\n%s", role)
		}
	})
}

func TestR2RenderedRoleAlwaysCarriesTheBoundary(t *testing.T) {
	p := platform(t)
	for _, env := range p.EnvOrder {
		role := object(t, Evaluate(spec(t, base), p, env), "iam-role.yaml")
		if !strings.Contains(role, "permissionsBoundary: "+p.Envs[env].Config.PermissionsBoundaryARN) {
			t.Fatalf("%s role has no boundary:\n%s", env, role)
		}
	}
}

func TestR3(t *testing.T) {
	p := platform(t)
	t.Run("another team's parameter", func(t *testing.T) {
		expectError(t, p, with("ssm: /ingestion/odds-ingest/vendor-api-key", "ssm: /state/odds-ingest/vendor-api-key"),
			"R3", "secrets[0].ssm", "/<env>/ingestion/odds-ingest/vendor-api-key")
	})
	t.Run("path with the environment in it", func(t *testing.T) {
		expectError(t, p, with("ssm: /ingestion/odds-ingest/vendor-api-key", "ssm: /prd/ingestion/odds-ingest/vendor-api-key"),
			"R3", "secrets[0].ssm", "ssm: /ingestion/odds-ingest/vendor-api-key")
	})
	t.Run("escaping the prefix", func(t *testing.T) {
		expectError(t, p, with("ssm: /ingestion/odds-ingest/vendor-api-key", "ssm: /ingestion/../state/x"), "R3", "secrets[0].ssm", "name one parameter")
	})
	t.Run("the platform injects the env prefix", func(t *testing.T) {
		for _, env := range p.EnvOrder {
			es := object(t, Evaluate(spec(t, base), p, env), "externalsecret.yaml")
			if !strings.Contains(es, "key: /"+env+"/ingestion/odds-ingest/vendor-api-key") {
				t.Fatalf("%s:\n%s", env, es)
			}
		}
	})
}

func TestR4(t *testing.T) {
	p := platform(t)
	t.Run("allow-all ingress", func(t *testing.T) {
		expectError(t, p, with("{namespace: monitoring, ports: [8080]}", "{ports: [8080]}"), "R4", "ingress[0]", "one rule per caller")
	})
	t.Run("internet egress on another port", func(t *testing.T) {
		expectError(t, p, with("ports: [443], reason: vendor feed", "ports: [80], reason: vendor feed"), "R4", "egress[1]", "ports: [443]")
	})
	t.Run("internet egress without a reason", func(t *testing.T) {
		expectError(t, p, with("ports: [443], reason: vendor feed", "ports: [443]"), "R4", "egress[1]", "reason")
	})
	t.Run("literal VPC CIDR", func(t *testing.T) {
		expectError(t, p, with(`cidr: "{vpcCidr}"`, "cidr: 10.20.4.0/24"), "R4", "egress[0].cidr", `cidr: "{vpcCidr}"`)
	})
	t.Run("literal external CIDR", func(t *testing.T) {
		expectError(t, p, with(`cidr: "{vpcCidr}"`, "cidr: 203.0.113.40/32"), "R4", "egress[0].cidr", "0.0.0.0/0, ports: [443]")
	})
	t.Run("no ports opens every port", func(t *testing.T) {
		expectError(t, p, with(`{cidr: "{vpcCidr}", ports: [9098]}`, `{cidr: "{vpcCidr}"}`), "R4", "egress[0].ports", "list the ports")
	})
	t.Run("spec cannot set the pod selector", func(t *testing.T) {
		expectError(t, p, base+"podSelector: {}\n", "R4", "podSelector", "remove it")
	})
	t.Run("rendered policy selects only this service and fills the VPC token", func(t *testing.T) {
		np := object(t, Evaluate(spec(t, base), p, "prd"), "networkpolicy.yaml")
		for _, want := range []string{"podSelector:\n    matchLabels:\n      app.kubernetes.io/name: odds-ingest", "cidr: 10.30.0.0/16", "except:\n              - 10.30.0.0/16"} {
			if !strings.Contains(np, want) {
				t.Errorf("policy lacks %q:\n%s", want, np)
			}
		}
	})
}

func TestR5(t *testing.T) {
	p := platform(t)
	t.Run("ARN as a resource", func(t *testing.T) {
		expectError(t, p, with(`["s3-object:trading-platform-{env}-odds-snapshots/*"]`, `["arn:aws:s3:::trading-platform-stg-odds-snapshots/*"]`),
			"R5", "permissions[0].resources[0]", "write s3-object:trading-platform-stg-odds-snapshots/*")
	})
	t.Run("account id anywhere", func(t *testing.T) {
		expectError(t, p, with("reason: vendor feed", "reason: vendor feed via 333333333333"), "R5", "egress[1].reason", "remove it")
	})
	t.Run("cluster name instead of the env token", func(t *testing.T) {
		expectError(t, p, with("trading-platform-{env}-odds-snapshots/*", "trading-platform-stg-odds-snapshots/*"), "R5", "permissions[0].resources[0]", "{env}")
	})
	t.Run("IP address in free text", func(t *testing.T) {
		expectError(t, p, with("reason: vendor feed", "reason: vendor at 198.51.100.7"), "R5", "egress[1].reason", "{vpcCidr}")
	})
}

// R6 (live in stg before prd) is a fact about time, not about a spec: the
// promotion package decides it. What render owes R6 is that each environment's
// chart and image come only from that environment's promoted pointers.
func TestChartAndImageComeOnlyFromTheEnvironmentsPointers(t *testing.T) {
	p := platform(t)
	sw := strings.NewReplacer("odds-ingest", "settlement-writer", "team: ingestion", "team: state", "/ingestion/", "/state/").Replace(base)
	for env, want := range map[string][]string{
		"stg": {"targetRevision: 2.7.1", "tag: r7", "222222222222.dkr.ecr.us-east-1.amazonaws.com/trading/settlement-writer"},
		"prd": {"targetRevision: 2.7.0", "tag: r6", "333333333333.dkr.ecr.us-east-1.amazonaws.com/trading/settlement-writer"},
	} {
		app := object(t, Evaluate(spec(t, sw), p, env), "application.yaml")
		for _, w := range want {
			if !strings.Contains(app, w) {
				t.Errorf("%s application lacks %q:\n%s", env, w, app)
			}
		}
	}
	t.Run("a service Kargo has not promoted has no chart Application yet", func(t *testing.T) {
		r := Evaluate(spec(t, base), p, "prd")
		if len(r.Notes) != 1 || !strings.Contains(r.Notes[0], "Kargo has not promoted") {
			t.Fatal(r.Notes)
		}
	})
}

func TestR7(t *testing.T) {
	p := platform(t)
	t.Run("environments must be explicit", func(t *testing.T) {
		expectError(t, p, with("environments: [stg, prd]\n", ""), "R7", "environments", "environments: [stg, prd]")
	})
	t.Run("prd requires stg", func(t *testing.T) {
		expectError(t, p, with("environments: [stg, prd]", "environments: [prd]"), "R7", "environments", "keep stg")
	})
	t.Run("unknown environment", func(t *testing.T) {
		expectError(t, p, with("environments: [stg, prd]", "environments: [stg, qa]"), "R7", "environments[1]", "stg, prd")
	})
	t.Run("stg only is fine and renders nothing for prd", func(t *testing.T) {
		r := Evaluate(spec(t, with("environments: [stg, prd]", "environments: [stg]")), p, "prd")
		if r.Errors() != 0 || len(r.Objects) != 0 {
			t.Fatalf("%v %d", r.Findings, len(r.Objects))
		}
	})
}

func TestR8(t *testing.T) {
	p := platform(t)
	sw := strings.NewReplacer("odds-ingest", "settlement-writer", "team: ingestion", "team: state", "/ingestion/", "/state/").Replace(base)
	grantsAllButWrite := sw + `  - actions: [kafka-cluster:Connect]
    resources: [msk-cluster:main]
  - actions: [kafka-cluster:DescribeTopic, kafka-cluster:ReadData]
    resources: [msk-topic:main/settlement-instructions, msk-topic:main/pp-settlement-events]
  - actions: [kafka-cluster:DescribeGroup, kafka-cluster:AlterGroup]
    resources: [msk-group:main/settlement-writer]
  - actions: [rds-db:connect]
    resources: [rds-dbuser:settlements/settlement_writer]
  - actions: [s3:PutObject]
    resources: ["s3-object:trading-platform-{env}-settlement-reports/*"]
`
	t.Run("removing a used permission is refused with both ways out", func(t *testing.T) {
		fs := errorsFor(t, p, spec(t, grantsAllButWrite), "R8")
		if len(fs) != 2 || !strings.Contains(fs[0].Problem, "kafka-cluster:WriteData on msk-topic:main/pp-settlement-events") ||
			!strings.Contains(fs[0].Fix, "acknowledgeRemovals") || !strings.Contains(fs[0].Fix, "grant it") {
			t.Fatalf("got %v", fs)
		}
		if r := Evaluate(spec(t, grantsAllButWrite), p, "stg"); r.Verdict != Partial || object(t, r, "networkpolicy.yaml") == "" {
			t.Fatalf("R8 should hold the role but not the rest: %s", r.Verdict)
		}
		for _, o := range Evaluate(spec(t, grantsAllButWrite), p, "stg").Objects {
			if o.Name == "iam-role.yaml" {
				t.Fatal("role rendered despite an unacknowledged removal")
			}
		}
	})
	t.Run("an acknowledged removal is allowed and stays visible", func(t *testing.T) {
		acked := grantsAllButWrite + `acknowledgeRemovals:
  - {action: kafka-cluster:WriteData, resource: msk-topic:main/pp-settlement-events, reason: moved to settlement-publisher}
`
		r := Evaluate(spec(t, acked), p, "stg")
		if r.Errors() != 0 {
			t.Fatal(r.Findings)
		}
		if !strings.Contains(r.Findings[0].Problem, "acknowledged: moved to settlement-publisher") {
			t.Fatalf("want a visible warning, got %v", r.Findings)
		}
	})
	t.Run("a running service without usage data is flagged", func(t *testing.T) {
		tu := strings.NewReplacer("odds-ingest", "trader-ui", "team: ingestion", "team: distribution", "/ingestion/", "/distribution/").Replace(base)
		p := platform(t)
		delete(p.Envs["prd"].Observed, "trader-ui")
		r := Evaluate(spec(t, tu), p, "prd")
		if len(r.Findings) != 1 || !strings.Contains(r.Findings[0].Problem, "no usage data") {
			t.Fatal(r.Findings)
		}
	})
	t.Run("dropping an environment the service runs in is shown", func(t *testing.T) {
		sw := strings.NewReplacer("odds-ingest", "settlement-writer", "team: ingestion", "team: state", "/ingestion/", "/state/").Replace(base)
		r := Evaluate(spec(t, strings.Replace(sw, "environments: [stg, prd]", "environments: [stg]", 1)), p, "prd")
		if len(r.Findings) != 1 || r.Findings[0].Rule != "R8" || !strings.Contains(r.Findings[0].Problem, "are deleted") {
			t.Fatal(r.Findings)
		}
	})
}

func TestUnknownCatalogNameIsRefused(t *testing.T) {
	p := platform(t)
	expectError(t, p, base+"  - actions: [kafka-cluster:Connect]\n    resources: [msk-cluster:analytics]\n", "REF", "permissions[1].resources[0]", "")
}

func TestUnknownFieldIsRefused(t *testing.T) {
	p := platform(t)
	expectError(t, p, base+"replicas: 3\n", "SCHEMA", "replicas", "allowed fields")
}

func TestNetworkAdvice(t *testing.T) {
	p := platform(t)
	warns := func(yaml string) []Finding {
		var out []Finding
		for _, f := range Evaluate(spec(t, yaml), p, "stg").Findings {
			if f.Rule == "NET" {
				out = append(out, f)
			}
		}
		return out
	}
	if w := warns(with("{namespace: monitoring, ports: [8080]}", "{namespace: kube-system, ports: [3000], reason: internal ALB}")); len(w) != 1 || !strings.Contains(w[0].Fix, `cidr: "{vpcCidr}"`) {
		t.Errorf("kube-system ingress: %v", w)
	}
	if w := warns(with("  - {cidr: 0.0.0.0/0, ports: [443], reason: vendor feed}\n", "")); len(w) != 1 || !strings.Contains(w[0].Problem, "gateway endpoint") {
		t.Errorf("S3 without egress: %v", w)
	}
}

func TestFleetFindsOneSidedRules(t *testing.T) {
	p := platform(t)
	server := spec(t, strings.NewReplacer("odds-ingest", "settlement-writer", "team: ingestion", "team: state", "/ingestion/", "/state/").Replace(base))
	server.Ingress = append(server.Ingress, Peer{Namespace: "distribution", Ports: []int{9090}})
	caller := spec(t, strings.NewReplacer("odds-ingest", "trader-ui", "team: ingestion", "team: distribution", "/ingestion/", "/distribution/").Replace(base))
	if got := CheckFleet([]*Spec{server, caller}, p); len(got["settlement-writer"]) != 1 {
		t.Fatalf("want one-sided ingress reported, got %v", got)
	}
	caller.Egress = append(caller.Egress, Peer{Namespace: "state", Ports: []int{9090}})
	if got := CheckFleet([]*Spec{server, caller}, p); len(got) != 0 {
		t.Fatalf("matched rules reported: %v", got)
	}
}

func object(t *testing.T, r Result, name string) string {
	t.Helper()
	for _, o := range r.Objects {
		if o.Name == name {
			b, err := o.YAML()
			if err != nil {
				t.Fatal(err)
			}
			return string(b)
		}
	}
	t.Fatalf("%s/%s: no %s (verdict %s, findings %v)", r.Env, r.Service, name, r.Verdict, r.Findings)
	return ""
}
