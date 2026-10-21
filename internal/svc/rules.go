package svc

import (
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// Check runs every rule that does not depend on one environment's facts.
// Each rule is one function, so a rule can be read, tested and changed alone.
func Check(s *Spec, p *Platform) []Finding {
	var fs []Finding
	for _, rule := range []func(*Spec, *Platform) []Finding{checkSchema, checkR1, checkR2, checkOwnership, checkR3, checkR4, checkR5, checkR7, checkRemovals} {
		fs = append(fs, rule(s, p)...)
	}
	return fs
}

// CheckEnv runs the checks that need one environment's catalog and usage data.
func CheckEnv(s *Spec, p *Platform, env string) []Finding {
	e := p.Envs[env]
	var fs []Finding
	fs = append(fs, checkRefs(s, e)...)
	fs = append(fs, checkR8(s, p, env)...)
	fs = append(fs, checkEnvRemoval(s, p, env)...)
	fs = append(fs, checkS3Reachable(s, e)...)
	for i := range fs {
		fs[i].Envs = []string{env}
	}
	return fs
}

// team is the team the registry gives the spec's repository; the spec's own
// claim is used only when the repository is not registered (R1 refuses that).
func team(s *Spec, p *Platform) string {
	if _, r, ok := p.Lookup(s.Repo); ok {
		return r.Team
	}
	return s.Team
}

// ---- schema -----------------------------------------------------------------

// reserved are fields a spec may not set because the platform owns them. They
// get a rule-specific answer instead of "unknown field".
var reserved = map[string]struct{ rule, why string }{
	"namespace":           {"R1", "the platform deploys into your team's namespace"},
	"project":             {"R1", "the platform deploys under your team's Argo CD project"},
	"argocdProject":       {"R1", "the platform deploys under your team's Argo CD project"},
	"destination":         {"R1", "the platform chooses the cluster and namespace"},
	"chart":               {"R1", "the registry names the chart your repository ships"},
	"image":               {"R1", "the registry names the image your repository ships"},
	"podSelector":         {"R4", "the platform selects your pods by app.kubernetes.io/name"},
	"accountId":           {"R5", "account ids live only in platform config"},
	"roleArn":             {"R5", "the platform names and creates your role"},
	"clusterName":         {"R5", "cluster names live only in platform config"},
	"vpcCidr":             {"R5", "write the {vpcCidr} token in a network rule instead"},
	"permissionsBoundary": {"R2", "the platform attaches the boundary to every role"},
}

func checkSchema(s *Spec, p *Platform) []Finding {
	var fs []Finding
	for _, k := range sortedKeys(s.Unknown) {
		if r, ok := reserved[k]; ok {
			fs = append(fs, errorf(r.rule, k, reservedFix(k, s, p), "%q is set by the platform, not the spec: %s", k, r.why))
			continue
		}
		fs = append(fs, errorf("SCHEMA", k, "remove it; allowed fields are service, team, environments, ingress, egress, permissions, secrets, acknowledgeRemovals", "unknown field %q", k))
	}
	for name, v := range map[string]string{"service": s.Service, "team": s.Team} {
		if v == "" {
			fs = append(fs, errorf("SCHEMA", name, fmt.Sprintf("add %s: <value>", name), "%s is required", name))
		}
	}
	nested := func(path string, unknown map[string]yaml.Node) {
		for _, k := range sortedKeys(unknown) {
			fs = append(fs, errorf("SCHEMA", path+"."+k, "remove it", "unknown field %q", k))
		}
	}
	for i, r := range s.Ingress {
		nested(fmt.Sprintf("ingress[%d]", i), r.Unknown)
	}
	for i, r := range s.Egress {
		nested(fmt.Sprintf("egress[%d]", i), r.Unknown)
	}
	for i, r := range s.Permissions {
		nested(fmt.Sprintf("permissions[%d]", i), r.Unknown)
	}
	for i, r := range s.Secrets {
		nested(fmt.Sprintf("secrets[%d]", i), r.Unknown)
	}
	for i, r := range s.AcknowledgeRemovals {
		nested(fmt.Sprintf("acknowledgeRemovals[%d]", i), r.Unknown)
	}
	return fs
}

func reservedFix(field string, s *Spec, p *Platform) string {
	switch {
	case reserved[field].rule != "R1":
		return "remove it"
	case field == "chart" || field == "image":
		return fmt.Sprintf("remove it; to change it, edit your service's entry in platform-gitops teams/%s/services.yaml", team(s, p))
	}
	t := team(s, p)
	return fmt.Sprintf("remove it. %s deploys into namespace %s under Argo CD project %s, because the registry assigns it to team %s. "+
		"If that project rejects a resource your chart contains (a ClusterRole, say), change the chart: team projects accept namespaced kinds only, so use a Role and RoleBinding",
		s.Service, t, t, t)
}

// ---- R1: own namespace, own project ----------------------------------------

func checkR1(s *Spec, p *Platform) []Finding {
	name, reg, ok := p.Lookup(s.Repo)
	if !ok {
		return []Finding{errorf("R1", "service", fmt.Sprintf("register the service in platform-gitops teams/%s/services.yaml with repo: %s; your team leads approve that, not the platform team", s.Team, s.Repo),
			"repository %q is not registered, so it has no service, team or namespace", s.Repo)}
	}
	var fs []Finding
	if s.Service != name {
		fs = append(fs, errorf("R1", "service", fmt.Sprintf("service: %s. A repository renders only the service registered to it", name),
			"repository %s is registered as service %s, but this spec says %q", s.Repo, name, s.Service))
	}
	if s.Team != reg.Team {
		fs = append(fs, errorf("R1", "team", fmt.Sprintf("set team: %s. To move the service to another team, change the registry entry (both teams' leads approve)", reg.Team),
			"the registry assigns %s to team %s, not %s", name, reg.Team, s.Team))
	}
	for _, env := range p.EnvOrder {
		cfg := p.Envs[env].Config
		t, ok := cfg.Teams[reg.Team]
		if !ok || !slices.Contains(cfg.ArgoCD.AllowedProjects, t.Project) {
			fs = append(fs, errorf("R1", "team", "this is a platform configuration gap, not yours: the platform team adds the team to platform-config",
				"team %s has no namespace and allowed project in %s", reg.Team, env))
		}
	}
	return fs
}

// ---- R2: IAM ------------------------------------------------------------------

// kmsUse are the KMS actions that use a key. Every other KMS action manages
// keys or who may use them, which R2 forbids.
var kmsUse = []string{"Decrypt", "Encrypt", "GenerateDataKey", "GenerateDataKeyWithoutPlaintext", "GenerateDataKeyPair",
	"GenerateDataKeyPairWithoutPlaintext", "ReEncryptFrom", "ReEncryptTo", "DescribeKey", "Sign", "Verify", "GenerateMac", "VerifyMac", "GetPublicKey"}

var kmsManage = []string{"CreateKey", "CreateGrant", "RetireGrant", "RevokeGrant", "PutKeyPolicy", "ScheduleKeyDeletion", "CancelKeyDeletion",
	"DisableKey", "EnableKey", "CreateAlias", "DeleteAlias", "UpdateAlias", "UpdateKeyDescription", "EnableKeyRotation", "DisableKeyRotation",
	"RotateKeyOnDemand", "ImportKeyMaterial", "DeleteImportedKeyMaterial", "TagResource", "UntagResource", "ReplicateKey", "UpdatePrimaryRegion"}

func checkR2(s *Spec, _ *Platform) []Finding {
	var fs []Finding
	for i, perm := range s.Permissions {
		base := fmt.Sprintf("permissions[%d]", i)
		if len(perm.Actions) == 0 {
			fs = append(fs, errorf("R2", base+".actions", "list the actions, e.g. actions: [s3:GetObject]", "a permission needs at least one action"))
		}
		if len(perm.Resources) == 0 {
			fs = append(fs, errorf("R2", base+".resources", "list the resources as platform references: "+refTypeNames(), "a permission needs at least one resource"))
		}
		resourceServices := map[string]bool{}
		for _, r := range perm.Resources {
			if rt, _, ok := splitRef(r); ok {
				resourceServices[rt.service] = true
			}
		}
		for j, a := range perm.Actions {
			path := fmt.Sprintf("%s.actions[%d]", base, j)
			f, svc := checkAction(path, a)
			fs = append(fs, f...)
			if len(f) == 0 && len(resourceServices) > 0 && !resourceServices[svc] {
				fs = append(fs, errorf("R2", path, "put each service's actions in a statement with that service's resources", "%s is a %s action, but no resource in this statement is a %s resource", a, svc, svc))
			}
		}
		for j, r := range perm.Resources {
			fs = append(fs, checkResource(fmt.Sprintf("%s.resources[%d]", base, j), r)...)
		}
	}
	return fs
}

// actionSyntax is what IAM action names look like. Anything else (a space, a
// look-alike Unicode letter) is refused rather than guessed at.
var actionSyntax = regexp.MustCompile(`^[A-Za-z0-9-]+:[A-Za-z0-9*?]+$`)

// controlActions change who can reach a resource or whether it exists, rather
// than use it. A family wildcard R2 allows (s3:Put*) still covers them, so a
// pattern is refused if it can match any of them. The boundary denies them too.
var controlActions = map[string][]string{
	// S3 has no Delete action for the public access block, ownership controls or
	// replication: the matching Put action authorises those deletes.
	"s3": {"PutBucketPolicy", "DeleteBucketPolicy", "PutBucketAcl", "PutObjectAcl", "PutObjectVersionAcl",
		"PutBucketPublicAccessBlock", "PutAccountPublicAccessBlock", "PutAccessPointPublicAccessBlock", "PutBucketOwnershipControls",
		"PutReplicationConfiguration", "DeleteBucket", "PutAccessPointPolicy", "DeleteAccessPointPolicy",
		"PutAccessPointPolicyForObjectLambda", "DeleteAccessPointPolicyForObjectLambda", "PutMultiRegionAccessPointPolicy"},
	"kafka-cluster": {"AlterCluster", "AlterClusterDynamicConfiguration", "AlterTopic", "AlterTopicDynamicConfiguration", "DeleteTopic", "DeleteGroup"},
}

// checkAction returns the findings for one action and its service prefix,
// lowercased: IAM matches action names case-insensitively, so must we.
func checkAction(path, a string) ([]Finding, string) {
	if a == "*" {
		return []Finding{errorf("R2", path, "list the actions you need, e.g. s3:GetObject, s3:PutObject", `"*" grants every action of every AWS service`)}, ""
	}
	if !actionSyntax.MatchString(a) {
		return []Finding{errorf("R2", path, "write <service>:<Action> with ASCII letters, digits and - only, e.g. s3:GetObject", "%q is not an IAM action", a)}, ""
	}
	svc, name, _ := strings.Cut(a, ":")
	svc = strings.ToLower(svc)
	if strings.ContainsAny(svc, "*?") {
		return []Finding{errorf("R2", path, "name one service, e.g. s3:GetObject", "%q has a wildcard in the service prefix, which spans services", a)}, svc
	}
	switch svc {
	case "iam":
		return []Finding{errorf("R2", path, "remove it; a pod never needs to manage IAM", "IAM actions are never grantable from a spec: they would let the pod change its own permissions")}, svc
	case "sts":
		return []Finding{errorf("R2", path,
			"remove it. Cross-account access is not self-service: ask the platform team for the data-account access you need (what, and why); they set up the trust on both sides, outside the spec",
			"STS actions are never grantable from a spec: assuming another role escapes this role's boundary")}, svc
	case "kms":
		if !kmsAllowed(name) {
			return []Finding{errorf("R2", path,
				"to read data encrypted with a key, grant kms:Decrypt; to write it, kms:GenerateDataKey; e.g. actions: [kms:Decrypt], resources: [kms-key:<key>]. S3 objects encrypted with SSE-KMS need only kms:Decrypt to be read",
				"%s manages a KMS key or who may use it (key management), which is never grantable from a spec", a)}, svc
		}
	}
	if strings.HasPrefix(name, "*") || strings.HasPrefix(name, "?") {
		return []Finding{errorf("R2", path,
			fmt.Sprintf("list the actions you need (e.g. %s:GetObject, %s:PutObject); a wildcard within one action family, such as %s:Get*, is allowed", svc, svc, svc),
			"%q grants every %s action; a wildcard may only narrow a family of actions", a, svc)}, svc
	}
	var hits []string
	for _, c := range controlActions[svc] {
		if globMatch(strings.ToLower(name), strings.ToLower(c)) {
			hits = append(hits, svc+":"+c)
		}
	}
	if len(hits) > 0 {
		fix := "list the data actions you need instead, e.g. s3:PutObject"
		if svc == "kafka-cluster" {
			fix = "list the data actions you need instead, e.g. kafka-cluster:WriteData; topic and cluster configuration belongs to the owning team's tooling"
		}
		return []Finding{errorf("R2", path, fix,
			"%s covers %s, which change who can reach the resource or whether it exists; a pod uses resources, it does not administer them", a, strings.Join(hits, ", "))}, svc
	}
	return nil, svc
}

func kmsAllowed(name string) bool {
	matchesAny := func(list []string) bool {
		return slices.ContainsFunc(list, func(n string) bool { return globMatch(strings.ToLower(name), strings.ToLower(n)) })
	}
	return matchesAny(kmsUse) && !matchesAny(kmsManage)
}

var arnS3 = regexp.MustCompile(`^arn:aws:s3:::([^/]+)(/.*)?$`)

func checkResource(path, r string) []Finding {
	if r == "*" {
		return []Finding{errorf("R2", path,
			"name each resource, e.g. s3-bucket:trading-platform-{env}-<bucket> and s3-object:trading-platform-{env}-<bucket>/*. When you need another one later, adding it is a one-line change you can merge yourself",
			`Resource "*" means every resource of these actions in the account`)}
	}
	if strings.HasPrefix(r, "arn:") {
		fix := "write a platform reference instead: " + refTypeNames()
		if m := arnS3.FindStringSubmatch(r); m != nil {
			fix = "write s3-bucket:" + m[1]
			if m[2] != "" {
				fix = "write s3-object:" + m[1] + m[2]
			}
		}
		return []Finding{errorf("R5", path, fix, "%q is an ARN; ARNs carry account ids, which never appear in a service repository", r)}
	}
	rt, name, ok := splitRef(r)
	if !ok {
		return []Finding{errorf("R2", path, "use one of: "+refTypeNames(), "%q is not a platform resource reference", r)}
	}
	head, _, _ := strings.Cut(name, "/")
	if name == "" || strings.ContainsAny(head, "*?") {
		return []Finding{errorf("R2", path, "name it exactly ("+rt.form+"); wildcards are allowed only after the first /, e.g. s3-object:<bucket>/reports/*",
			"%q has a wildcard in the resource name itself", r)}
	}
	return nil
}

// ---- R3: SSM parameters ---------------------------------------------------------

var secretKey = regexp.MustCompile(`^[-._a-zA-Z0-9]+$`)

func checkR3(s *Spec, p *Platform) []Finding {
	t := team(s, p)
	var fs []Finding
	seen := map[string]bool{}
	for i, sec := range s.Secrets {
		base := fmt.Sprintf("secrets[%d]", i)
		if !secretKey.MatchString(sec.Key) {
			fs = append(fs, errorf("SCHEMA", base+".key", "use letters, digits, '-', '_' or '.', e.g. key: db-password", "%q is not a valid Kubernetes Secret key", sec.Key))
		} else if seen[sec.Key] {
			fs = append(fs, errorf("SCHEMA", base+".key", "give each secret its own key", "key %q is used twice", sec.Key))
		}
		seen[sec.Key] = true

		path := base + ".ssm"
		segs := strings.Split(strings.TrimPrefix(sec.SSM, "/"), "/")
		switch {
		case !strings.HasPrefix(sec.SSM, "/"):
			fs = append(fs, errorf("R3", path, fmt.Sprintf("ssm: /%s/%s/<name>", t, s.Service), "%q must start with /<team>/", sec.SSM))
		case strings.ContainsAny(sec.SSM, "*?") || slices.Contains(segs, "..") || slices.Contains(segs, ""):
			fs = append(fs, errorf("R3", path, "name one parameter, e.g. ssm: /"+t+"/"+s.Service+"/<name>", "%q is not a single parameter path", sec.SSM))
		case p.Envs[segs[0]] != nil:
			fs = append(fs, errorf("R3", path, "ssm: /"+strings.Join(segs[1:], "/"),
				"%q starts with the environment; the platform adds /<env> itself, so one spec works in every environment", sec.SSM))
		case segs[0] != t:
			rest := strings.Join(segs[1:], "/")
			fs = append(fs, errorf("R3", path,
				fmt.Sprintf("create the parameter under your team as /<env>/%s/%s in each environment (team %s owns the current value and can copy it, or hand it to you), then write ssm: /%s/%s", t, rest, segs[0], t, rest),
				"%s belongs to team %s and may read only parameters under /<env>/%s/; %q is under team %s", s.Service, t, t, sec.SSM, segs[0]))
		case len(segs) < 2:
			fs = append(fs, errorf("R3", path, "ssm: /"+t+"/"+s.Service+"/<name>", "%q names a team, not a parameter", sec.SSM))
		}
	}
	return fs
}

// ---- R4: network ------------------------------------------------------------------

const (
	vpcToken = "{vpcCidr}"
	// albToken is the internal load balancers' subnets. On the VPC CNI pods have
	// VPC addresses, so ingress from {vpcCidr} admits every pod in the cluster too.
	albToken = "{albSubnets}"
)

func checkR4(s *Spec, p *Platform) []Finding {
	var fs []Finding
	for i, r := range s.Ingress {
		path := fmt.Sprintf("ingress[%d]", i)
		switch {
		case r.Namespace != "" && r.CIDR != "":
			fs = append(fs, errorf("R4", path, "split it into two rules", "a rule names a namespace or a cidr, not both"))
		case r.Namespace == "" && r.CIDR == "", r.Namespace != "" && !dnsLabel.MatchString(r.Namespace):
			fs = append(fs, errorf("R4", path,
				"add one rule per caller: - namespace: <caller's namespace>, ports: [<port>]. Adding a caller later is a one-line change you merge yourself",
				"an ingress rule must name the namespace traffic comes from; allowing all would let any pod in any namespace reach this service"))
		case r.CIDR != "" && r.CIDR != vpcToken && r.CIDR != albToken:
			fs = append(fs, literalCIDR(path+".cidr", r.CIDR, p, false))
		case r.CIDR != "" && !meaningful(r.Reason):
			fs = append(fs, errorf("R4", path+".reason", "add reason: <who connects, e.g. the internal ALB for the trader UI>", "ingress from addresses rather than a namespace needs a reason"))
		}
		if r.CIDR == vpcToken {
			fs = append(fs, warnf("NET", path+".cidr",
				fmt.Sprintf(`if the caller is the internal load balancer (IP targets), write - cidr: "%s", ports: %v, reason: %s`, albToken, r.Ports, r.Reason),
				"pods have VPC addresses on the VPC CNI, so ingress from {vpcCidr} admits every pod in the cluster too, not only what is outside it"))
		}
		if len(r.Ports) == 0 && (r.Namespace != "" || r.CIDR != "") {
			fs = append(fs, errorf("R4", path+".ports", "list the ports callers use, e.g. ports: [8080]", "no ports listed, which would open every port"))
		}
		if len(r.Hosts) > 0 {
			fs = append(fs, errorf("R4", path+".hosts", "remove it; hosts name the endpoints of an internet egress rule", "hosts are only meaningful on egress to 0.0.0.0/0"))
		}
		fs = append(fs, checkPorts(path, r.Ports)...)
		if r.Namespace == "kube-system" {
			fs = append(fs, warnf("NET", path+".namespace",
				fmt.Sprintf(`if the caller is the internal load balancer (IP targets), write - cidr: "%s", ports: %v, reason: %s`, albToken, r.Ports, r.Reason),
				"this admits pods running in kube-system. An AWS load balancer is not a pod: it connects from its own addresses in its subnets (with IP targets), so this rule will not admit it"))
		}
	}
	for i, r := range s.Egress {
		path := fmt.Sprintf("egress[%d]", i)
		switch {
		case r.Namespace != "" && r.CIDR != "":
			fs = append(fs, errorf("R4", path, "split it into two rules", "a rule names a namespace or a cidr, not both"))
		case r.Namespace == "" && r.CIDR == "":
			fs = append(fs, errorf("R4", path, `name where traffic goes: - namespace: <ns>, or - cidr: "{vpcCidr}" for the VPC`, "an egress rule must name a destination"))
		case r.Namespace != "" && !dnsLabel.MatchString(r.Namespace):
			fs = append(fs, errorf("R4", path+".namespace", "add one rule per namespace you call: - namespace: <its name>, ports: [<port>]",
				"%q is not a namespace name; egress must name each namespace it reaches", r.Namespace))
		case r.CIDR == "0.0.0.0/0":
			if !slices.Equal(r.Ports, []int{443}) || !meaningful(r.Reason) {
				fs = append(fs, errorf("R4", path,
					`internet egress is allowed only as - cidr: 0.0.0.0/0, ports: [443], hosts: [<the endpoints you call>], reason: <why>. Traffic inside the VPC needs its own rule: - cidr: "{vpcCidr}", ports: [<port>]`,
					"egress to 0.0.0.0/0 is allowed only on port 443 and only with a reason (got ports %v, reason %q)", r.Ports, r.Reason))
			}
			fs = append(fs, checkHosts(path+".hosts", r.Hosts)...)
		case r.CIDR == albToken:
			fs = append(fs, errorf("R4", path+".cidr", `for the VPC, write cidr: "{vpcCidr}"`, "{albSubnets} is for ingress from the internal load balancer only"))
		case r.CIDR != "" && r.CIDR != vpcToken:
			fs = append(fs, literalCIDR(path+".cidr", r.CIDR, p, true))
		}
		if len(r.Ports) == 0 && r.CIDR != "0.0.0.0/0" && (r.Namespace != "" || r.CIDR != "") {
			fs = append(fs, errorf("R4", path+".ports", "list the ports, e.g. ports: [5432]", "no ports listed, which would open every port"))
		}
		fs = append(fs, checkPorts(path, r.Ports)...)
		if len(r.Hosts) > 0 && r.CIDR != "0.0.0.0/0" {
			fs = append(fs, errorf("R4", path+".hosts", "remove it; hosts name the endpoints of an internet egress rule", "hosts are only meaningful on egress to 0.0.0.0/0"))
		}
	}
	return fs
}

var (
	dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$`)
	hostName = regexp.MustCompile(`^(\*\.)?([a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)
)

// awsHosts are AWS endpoints a spec names without knowing the region.
var awsHosts = map[string]bool{"aws:s3": true}

// checkHosts requires an internet rule to name what it calls. "reason: ." says
// nothing; a host list is checkable, shows up in review, and is exactly what a
// DNS-aware policy (an FQDN rule or an egress proxy) enforces.
func checkHosts(path string, hosts []string) []Finding {
	if len(hosts) == 0 {
		return []Finding{errorf("R4", path, "hosts: [<each endpoint you call, e.g. feed.vendor-example.com>]; for S3 through its endpoint, aws:s3",
			"internet egress must name the hosts it is for")}
	}
	var fs []Finding
	for i, h := range hosts {
		if !hostName.MatchString(h) && !awsHosts[h] {
			fs = append(fs, errorf("R4", fmt.Sprintf("%s[%d]", path, i), "write a DNS name, e.g. login.auth-provider.example (a leading *. is allowed), or aws:s3",
				"%q is not a host name", h))
		}
	}
	return fs
}

func checkPorts(path string, ports []int) []Finding {
	var fs []Finding
	for j, port := range ports {
		if port < 1 || port > 65535 {
			fs = append(fs, errorf("SCHEMA", fmt.Sprintf("%s.ports[%d]", path, j), "use a port between 1 and 65535", "%d is not a port", port))
		}
	}
	return fs
}

// literalCIDR explains what to write instead of a literal CIDR. Inside the VPC
// that is the token; outside it, an internet rule with a reason, because a
// NetworkPolicy cannot follow a provider's address changes and the address
// would also leak into the repository (R5).
func literalCIDR(path, cidr string, p *Platform, egress bool) Finding {
	inside := false
	if pfx, err := netip.ParsePrefix(cidr); err == nil {
		for _, env := range p.EnvOrder {
			if vpc, err := netip.ParsePrefix(p.Envs[env].Config.VPCCIDR); err == nil && vpc.Overlaps(pfx) {
				inside = true
			}
		}
	}
	fix := `it is inside the VPC: write cidr: "{vpcCidr}" (the platform fills in each environment's range), or "{albSubnets}" for the internal load balancer`
	if !inside && egress {
		fix = "it is outside the VPC: write - cidr: 0.0.0.0/0, ports: [443], reason: <the endpoint, e.g. auth provider login.example.com>"
	} else if !inside {
		fix = "ingress from outside the VPC arrives through the internal load balancer: write cidr: \"{albSubnets}\" with a reason"
	}
	return errorf("R4", path, fix, "%q is a literal CIDR; network rules use the {vpcCidr} token or 0.0.0.0/0 on 443 (and CIDRs never appear in a service repository, R5)", cidr)
}

// ---- R5: no environment facts in the repository -----------------------------------

var (
	reAccount = regexp.MustCompile(`(^|[^0-9])[0-9]{12}([^0-9]|$)`)
	reARN     = regexp.MustCompile(`arn:aws[a-z-]*:`)
	reIP      = regexp.MustCompile(`(^|[^0-9.])[0-9]{1,3}(\.[0-9]{1,3}){3}(/[0-9]{1,2})?`)
	// Fields with their own, more specific check: CIDRs (R4) and ARNs given as
	// resources (checkResource suggests the reference to write instead).
	reCIDRPath     = regexp.MustCompile(`^(ingress|egress)\[\d+\]\.cidr$`)
	reResourcePath = regexp.MustCompile(`^permissions\[\d+\]\.resources\[\d+\]$`)
)

func checkR5(s *Spec, p *Platform) []Finding {
	var known []string // facts from platform config that must not be copied
	for _, env := range p.EnvOrder {
		c := p.Envs[env].Config
		known = append(known, c.ClusterName, c.ImageRegistry, strings.TrimPrefix(c.ChartRegistry, "oci://"), c.PermissionsBoundaryARN)
	}
	var fs []Finding
	walkComments(&s.Raw, func(line int, c string) {
		for _, f := range []struct {
			re   *regexp.Regexp
			what string
		}{{reARN, "an ARN"}, {reAccount, "what looks like an AWS account id"}, {reIP, "an IP address or CIDR"}} {
			if f.re.MatchString(c) {
				fs = append(fs, errorf("R5", fmt.Sprintf("comment on line %d", line), "remove it; comments are in the repository too", "contains %s", f.what))
				return
			}
		}
		for _, k := range known {
			if k != "" && strings.Contains(c, k) {
				fs = append(fs, errorf("R5", fmt.Sprintf("comment on line %d", line), "remove it; comments are in the repository too", "contains %q, an environment-specific name from platform config", k))
				return
			}
		}
	})
	walk(&s.Raw, "", func(path, v string) {
		if reCIDRPath.MatchString(path) || (reResourcePath.MatchString(path) && strings.HasPrefix(v, "arn:")) {
			return
		}
		switch {
		case reARN.MatchString(v):
			fs = append(fs, errorf("R5", path, "remove it; the platform names roles, boundaries and resources", "contains an ARN"))
		case reAccount.MatchString(v):
			fs = append(fs, errorf("R5", path, "remove it; the platform knows each environment's account", "contains what looks like an AWS account id"))
		case reIP.MatchString(v) && !strings.Contains(v, "0.0.0.0/0"):
			fs = append(fs, errorf("R5", path, `use the {vpcCidr} token in a network rule`, "contains an IP address or CIDR"))
		default:
			for _, k := range known {
				if k != "" && strings.Contains(v, k) {
					fs = append(fs, errorf("R5", path, "write {env} where the value differs per environment; the platform knows cluster and registry names",
						"contains %q, an environment-specific name from platform config", k))
					break
				}
			}
		}
	})
	return fs
}

// walk calls fn for every scalar value in the tree with its spec path.
func walk(n *yaml.Node, path string, fn func(path, value string)) {
	switch n.Kind {
	case yaml.DocumentNode:
		for _, c := range n.Content {
			walk(c, path, fn)
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			key := n.Content[i].Value
			if path != "" {
				key = path + "." + key
			}
			walk(n.Content[i+1], key, fn)
		}
	case yaml.SequenceNode:
		for i, c := range n.Content {
			walk(c, fmt.Sprintf("%s[%d]", path, i), fn)
		}
	case yaml.ScalarNode:
		fn(path, n.Value)
	}
}

// meaningful is the least a reason must be: a few words, not "." or "x".
// Whether it is true is for review; whether it is there is checked here.
func meaningful(reason string) bool { return len(strings.Fields(reason)) >= 2 }

// walkComments calls fn for every comment in the tree with the line it is on.
func walkComments(n *yaml.Node, fn func(line int, comment string)) {
	for _, c := range []string{n.HeadComment, n.LineComment, n.FootComment} {
		if c != "" {
			fn(n.Line, c)
		}
	}
	for _, c := range n.Content {
		walkComments(c, fn)
	}
}

// ---- R8: acknowledgements are exact -----------------------------------------------------

// checkRemovals keeps an acknowledgement as narrow as the removal it confirms:
// one exact action on one exact resource, with a reason. A wildcard would
// confirm every future removal too, and R8 would never fire again.
func checkRemovals(s *Spec, _ *Platform) []Finding {
	var fs []Finding
	for i, r := range s.AcknowledgeRemovals {
		path := fmt.Sprintf("acknowledgeRemovals[%d]", i)
		if !actionSyntax.MatchString(r.Action) || strings.ContainsAny(r.Action, "*?") {
			fs = append(fs, errorf("R8", path+".action", "name the one action being removed, as the R8 finding printed it, e.g. kafka-cluster:WriteData", "%q is not one exact action", r.Action))
		}
		if _, _, ok := splitRef(r.Resource); !ok || strings.ContainsAny(r.Resource, "*?") {
			fs = append(fs, errorf("R8", path+".resource", "name the one resource, as the R8 finding printed it, e.g. msk-topic:main/pp-settlement-events", "%q is not one exact resource reference", r.Resource))
		}
		if !meaningful(r.Reason) {
			fs = append(fs, errorf("R8", path+".reason", "say why the removal is safe, e.g. reason: moved to the settlement-events-v2 topic", "%q does not say why the removal is safe", r.Reason))
		}
	}
	return fs
}

// ---- R7: environments are explicit ----------------------------------------------------

func checkR7(s *Spec, p *Platform) []Finding {
	if len(s.Environments) == 0 {
		return []Finding{errorf("R7", "environments", "environments: ["+strings.Join(p.EnvOrder, ", ")+"]", "a service must list every environment it runs in; there is no default")}
	}
	var fs []Finding
	seen := map[string]bool{}
	for i, env := range s.Environments {
		path := fmt.Sprintf("environments[%d]", i)
		if p.Envs[env] == nil {
			fs = append(fs, errorf("R7", path, "use one of: "+strings.Join(p.EnvOrder, ", "), "unknown environment %q", env))
		} else if seen[env] {
			fs = append(fs, errorf("R7", path, "list it once", "%s is listed twice", env))
		}
		seen[env] = true
	}
	for _, env := range p.EnvOrder {
		from := p.Envs[env].Config.Promotion.From
		if seen[env] && from != "" && !seen[from] {
			fs = append(fs, errorf("R7", "environments",
				fmt.Sprintf("keep %s: environments: [%s, %s]. Every %s change soaks in %s first, so %s cannot exist without it. To cut %s cost, run fewer or smaller replicas there in your chart's %s values",
					from, from, env, env, from, env, from, from),
				"%s is listed without %s", env, from))
		}
	}
	return fs
}

// ---- environment-specific checks -------------------------------------------------------

func checkRefs(s *Spec, e *Env) []Finding {
	var fs []Finding
	for i, perm := range s.Permissions {
		for j, r := range perm.Resources {
			rt, name, ok := splitRef(r)
			if !ok {
				continue // R2 already reports it
			}
			if _, err := rt.arn(expandEnv(name, e.Config.Env), e); err != nil {
				fs = append(fs, errorf("REF", fmt.Sprintf("permissions[%d].resources[%d]", i, j), "", "%v", err))
			}
		}
	}
	return fs
}

// checkR8 compares what the running pod is observed to use with what the spec
// grants. Anything used and no longer granted must be granted again or
// explicitly acknowledged; acknowledged removals stay visible as warnings.
func checkR8(s *Spec, p *Platform, env string) []Finding {
	obs := p.Envs[env].Observed[s.Service]
	if obs == nil {
		if p.Envs[env].Pointers[s.Service] == nil {
			return nil // not running here yet: nothing can be removed from it
		}
		return []Finding{warnf("R8", "permissions", "",
			"%s runs here but there is no usage data for it, so a permission this spec removes cannot be detected; review the role diff by hand until the usage collector reports", s.Service)}
	}
	var fs []Finding
	for _, u := range obs.Used {
		res := expandEnv(u.Resource, env)
		if granted(s, u.Action, res, env) {
			continue
		}
		if ack, ok := acknowledged(s, u.Action, res, env); ok {
			fs = append(fs, warnf("R8", "acknowledgeRemovals", "",
				"removes %s on %s, which %s used in %s (acknowledged: %s)", u.Action, u.Resource, obs.Identity, obs.Window, ack.Reason))
			continue
		}
		fs = append(fs, errorf("R8", "permissions",
			fmt.Sprintf("if the pod still needs it, grant it: - actions: [%s], resources: [%q]. If it is really unused now, confirm the removal: acknowledgeRemovals: - {action: %s, resource: %q, reason: <why it is safe>}",
				u.Action, u.Resource, u.Action, u.Resource),
			"the running pod used %s on %s through %s (%s), and this spec does not grant it; applying it would make that call fail",
			u.Action, u.Resource, obs.Identity, obs.Window))
	}
	return fs
}

func granted(s *Spec, action, resource, env string) bool {
	for _, perm := range s.Permissions {
		a := slices.ContainsFunc(perm.Actions, func(p string) bool { return globMatch(strings.ToLower(p), strings.ToLower(action)) })
		r := slices.ContainsFunc(perm.Resources, func(p string) bool { return globMatch(expandEnv(p, env), resource) })
		if a && r {
			return true
		}
	}
	return false
}

func acknowledged(s *Spec, action, resource, env string) (Removal, bool) {
	for _, r := range s.AcknowledgeRemovals {
		if strings.EqualFold(r.Action, action) && globMatch(expandEnv(r.Resource, env), resource) && r.Reason != "" {
			return r, true
		}
	}
	return Removal{}, false
}

// checkEnvRemoval warns when a spec stops listing an environment the service
// already runs in: everything it has there is deleted.
func checkEnvRemoval(s *Spec, p *Platform, env string) []Finding {
	if slices.Contains(s.Environments, env) || p.Envs[env].Pointers[s.Service] == nil {
		return nil
	}
	return []Finding{warnf("R8", "environments", "if this is intended there is nothing to do; to keep it, add "+env+" to environments",
		"%s runs in %s today; without %s in environments, its deployment, role, network policy and secret there are deleted", s.Service, env, env)}
}

// checkS3Reachable catches a spec that passes every rule and still cannot work:
// through a VPC gateway endpoint, S3 is reached at S3's public address ranges,
// which the {vpcCidr} token does not cover.
func checkS3Reachable(s *Spec, e *Env) []Finding {
	if e.Catalog.Endpoints.S3 != "gateway" {
		return nil
	}
	usesS3 := slices.ContainsFunc(s.Permissions, func(p Permission) bool {
		return slices.ContainsFunc(p.Actions, func(a string) bool { return strings.HasPrefix(strings.ToLower(a), "s3:") })
	})
	var internet *Peer
	for i, r := range s.Egress {
		if r.CIDR == "0.0.0.0/0" && slices.Equal(r.Ports, []int{443}) {
			internet = &s.Egress[i]
		}
	}
	switch {
	case !usesS3:
		return nil
	case internet == nil:
		return []Finding{warnf("NET", "egress",
			"add - cidr: 0.0.0.0/0, ports: [443], hosts: [aws:s3], reason: S3 through the VPC gateway endpoint",
			"this service calls S3, but no egress rule reaches it: S3 is behind a gateway endpoint here, whose addresses are S3's public ranges, not {vpcCidr}")}
	case !slices.Contains(internet.Hosts, "aws:s3"):
		return []Finding{warnf("NET", "egress",
			"add aws:s3 to that rule's hosts",
			"this service calls S3 through its internet egress rule (S3 is behind a gateway endpoint here, at S3's public addresses), but the rule's hosts do not say so; under host-based enforcement S3 calls would fail")}
	}
	return nil
}
