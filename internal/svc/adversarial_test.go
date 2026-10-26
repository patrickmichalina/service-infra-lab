package svc

import (
	"strings"
	"testing"
)

// Each case is an attack from the adversarial pass (docs/notes.md) that the
// renderer once accepted, or refused only by accident with the wrong message.
func TestAdversarialSpecsAreRefusedForTheRightReason(t *testing.T) {
	p := platform(t)
	perm := func(action, resource string) string {
		return base + "  - actions: [" + action + "]\n    resources: [" + resource + "]\n"
	}
	cases := []struct{ name, yaml, rule, problem string }{
		{"A1 IAM in upper case (IAM ignores case)", perm(`"IAM:PassRole"`, `"s3-bucket:trading-platform-{env}-odds-snapshots"`), "R2", "IAM actions are never grantable"},
		{"A2 STS in mixed case", perm(`"Sts:AssumeRole"`, `"s3-bucket:trading-platform-{env}-odds-snapshots"`), "R2", "STS actions are never grantable"},
		{"A3 KMS key management in upper case", perm(`"KMS:CreateGrant"`, `kms-key:ui-assets`), "R2", "key management"},
		{"A4 a space before the action", perm(`" iam:PassRole"`, `"s3-bucket:trading-platform-{env}-odds-snapshots"`), "R2", "is not an IAM action"},
		{"A4b a look-alike Unicode letter", perm(`"іam:PassRole"`, `"s3-bucket:trading-platform-{env}-odds-snapshots"`), "R2", "is not an IAM action"},
		{"A5 a family wildcard covering bucket policy", perm(`"s3:Put*"`, `"s3-bucket:trading-platform-{env}-odds-snapshots"`), "R2", "s3:PutBucketPolicy"},
		{"A5b topic administration", perm(`kafka-cluster:DeleteTopic`, `"msk-topic:main/odds.raw"`), "R2", "kafka-cluster:DeleteTopic"},
		{"A6 another team's bucket", perm(`s3:GetObject`, `"s3-object:trading-platform-{env}-settlement-reports/*"`), "R2", "belongs to team state, which has not shared it"},
		{"A6b a bucket nobody owns", perm(`s3:GetObject`, `"s3-object:trading-platform-{env}-unknown/*"`), "R2", "no team is recorded as the owner"},
		{"A7 every topic on the cluster", perm(`kafka-cluster:WriteData`, `"msk-topic:main/*"`), "R2", "spans resources of more than one owner"},
		{"A7b another team's topic", perm(`kafka-cluster:WriteData`, `"msk-topic:main/rfq.requests"`), "R2", "belongs to team normalization"},
		{"A7c another service's consumer group", perm(`kafka-cluster:AlterGroup`, `"msk-group:main/settlement-writer"`), "R2", "not this service's consumer group"},
		{"A8 any database user", perm(`rds-db:connect`, `"rds-dbuser:settlements/*"`), "R2", "connects only as odds_ingest"},
		{"A8b the admin database user", perm(`rds-db:connect`, `"rds-dbuser:settlements/postgres"`), "R2", "connects only as odds_ingest"},
		{"A10 an account id in a comment", "# deploys to 333333333333\n" + base, "R5", "account id"},
		{"A10b a CIDR in a line comment", strings.Replace(base, "ports: [9098]}", "ports: [9098]} # 10.20.0.0/16", 1), "R5", "IP address or CIDR"},
		{"A11 acknowledging every resource", base + "acknowledgeRemovals:\n  - {action: s3:PutObject, resource: \"*\", reason: no longer needed}\n", "R8", "not one exact resource"},
		{"A11b acknowledging every action", base + "acknowledgeRemovals:\n  - {action: \"s3:*\", resource: \"s3-bucket:trading-platform-{env}-odds-snapshots\", reason: no longer needed}\n", "R8", "not one exact action"},
		{"A11c acknowledging without a reason", base + "acknowledgeRemovals:\n  - {action: s3:PutObject, resource: \"s3-bucket:trading-platform-{env}-odds-snapshots\", reason: x}\n", "R8", "does not say why"},
		{"A12 internet egress with a one-character reason", with("reason: vendor feed", `reason: "."`), "R4", "only with a reason"},
		{"A12b internet egress without hosts", with("hosts: [feed.vendor-example.com, aws:s3], ", ""), "R4", "must name the hosts"},
		{"A12c an IP address as a host", with("hosts: [feed.vendor-example.com, aws:s3]", "hosts: [203.0.113.40]"), "R5", "IP address"},
		{"A14 egress to every namespace", with(`{cidr: "{vpcCidr}", ports: [9098]}`, `{namespace: "*", ports: [9098]}`), "R4", "is not a namespace name"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := spec(t, c.yaml)
			for _, env := range p.EnvOrder {
				for _, f := range Evaluate(s, p, env).Findings {
					if f.Severity == Error && f.Rule == c.rule && strings.Contains(f.Problem, c.problem) {
						return
					}
				}
			}
			t.Fatalf("want an %s error mentioning %q; got %v", c.rule, c.problem, Evaluate(s, p, "stg").Findings)
		})
	}
}

// Merge keys and alias bombs are YAML features an attacker controls; the
// parser must neither be fooled by nor stall on them.
func TestYAMLTricks(t *testing.T) {
	p := platform(t)
	expectError(t, p, base+"<<: {project: default}\n", "R1", "project", "")
	bomb := "a: &a [x,x,x,x,x,x,x,x,x,x]\nb: &b [*a,*a,*a,*a,*a,*a,*a,*a,*a,*a]\nc: &c [*b,*b,*b,*b,*b,*b,*b,*b,*b,*b]\n" +
		"d: &d [*c,*c,*c,*c,*c,*c,*c,*c,*c,*c]\ne: &e [*d,*d,*d,*d,*d,*d,*d,*d,*d,*d]\nf: &f [*e,*e,*e,*e,*e,*e,*e,*e,*e,*e]\ng: [*f,*f,*f,*f,*f,*f,*f,*f,*f,*f]\n"
	if s, err := ParseSpec("bomb.yaml", "example-org/odds-ingest", []byte(bomb)); err == nil {
		Evaluate(s, p, "stg") // must return; the test timeout catches a stall
	}
}

func TestOwnersShareForReadingOrWriting(t *testing.T) {
	p := platform(t)
	y := base + "  - actions: [s3:GetObject]\n    resources: [\"s3-object:trading-platform-{env}-settlement-reports/*\"]\n"
	q := *p
	q.Resources = append([]Ownership{{Ref: "s3-bucket:trading-platform-{env}-settlement-reports", Owner: "state",
		Share: []Share{{Service: "odds-ingest", Access: "read"}}}}, p.Resources...)
	if fs := errorsFor(t, &q, spec(t, y), "R2"); len(fs) != 0 {
		t.Fatalf("a read share admits reads: %v", fs)
	}
	w := strings.Replace(y, "s3:GetObject", "s3:PutObject", 1)
	expectError(t, &q, w, "R2", "permissions[1].resources[0]", "access: write")
}
