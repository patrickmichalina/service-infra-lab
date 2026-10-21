# Self-service service infrastructure

**Problem.** Every routine change waits about four days on a two-person platform team, which hand-writes five files and
reviews its own work. **Goal.** A routine change ships without a platform engineer in the path, and rules R1–R8 hold
anyway.

**Shape, in one paragraph.** A team writes a short **spec** in its own repository. The spec says what the service
needs, never how the platform provides it. A platform-owned CI job in `platform-gitops` reads that spec as untrusted
input and runs **`svcrender`**, which checks it against the rules and renders the five objects. The job commits the
result as a bot MR, and the MR **merges itself when the verdict is ACCEPT**. Argo CD applies it. Two more layers
refuse anything the renderer should have refused: **admission policies** in the cluster and **IAM conditions** in
AWS. **Kargo** promotes one Freight (spec commit + chart version + image tag) to prd. It does so only after the
**soak gate** (Part C) says that exact Freight has been live and healthy in stg. A human still presses sync in prd.

The platform team stops writing service files. It owns the renderer, the rules, admission, the AWS backstops, Kargo,
the gate and the catalog of shared resources. It also handles the exceptions that are not self-service by design:
cross-account access, cluster-scoped kinds, new platform namespaces. It reviews nothing routine.

```mermaid
flowchart LR
  subgraph svc["service repo (untrusted)"]
    spec[platform/service.yaml]
    chart[Helm chart]
  end
  subgraph pg["platform-gitops (trusted)"]
    reg["teams/&lt;team&gt;/services.yaml<br/>(team leads own)"]
    cat[catalog + platform-config<br/>(platform owns)]
    ptr[Kargo pointers]
    out[rendered/&lt;env&gt;/&lt;svc&gt;/]
  end
  spec -->|CI reads at a commit| R[svcrender<br/>check + render]
  reg --> R
  cat --> R
  ptr --> R
  R -->|bot MR, auto-merge on ACCEPT| out
  out --> A[Argo CD] --> K[(EKS: admission policies)]
  K --> ACK[ACK controllers] --> AWS[(AWS: boundary + IAM conditions)]
  chart --> A
```

## 1. What the engineer writes, and what they never have to know

There are two files, both reviewed by the team's own leads:

- **One registry line, once per service.** It lives in `platform-gitops/teams/<team>/services.yaml`, which that
  team's leads own through CODEOWNERS. It binds a repository to a service, a team, a chart and an image. It is
  the only place a service's identity lives, and the renderer looks it up by the **repository the spec came
  from**, not by anything the spec says ([`platform/registry.yaml`](../platform/registry.yaml)).
- **The spec,** at `platform/service.yaml` in the service repo. Here is request 01, complete
  ([`specs/odds-ingest.yaml`](../specs/odds-ingest.yaml)):

```yaml
service: odds-ingest
team: ingestion
environments: [stg, prd]
ingress:
  - {namespace: monitoring, ports: [8080], reason: Prometheus scrape}
egress:
  - {cidr: 0.0.0.0/0, ports: [443], hosts: [feed.vendor-example.com], reason: vendor odds feed}
  - {cidr: "{vpcCidr}", ports: [9098], reason: MSK brokers, IAM auth}
permissions:
  - {actions: [s3:PutObject],  resources: ["s3-object:trading-platform-{env}-odds-snapshots/*"]}
  - {actions: [s3:ListBucket], resources: ["s3-bucket:trading-platform-{env}-odds-snapshots"]}
  - {actions: [kafka-cluster:Connect, kafka-cluster:WriteDataIdempotently], resources: ["msk-cluster:main"]}
  - {actions: [kafka-cluster:DescribeTopic, kafka-cluster:WriteData], resources: ["msk-topic:main/odds.raw"]}
secrets:
  - {key: vendor-api-key, ssm: /ingestion/odds-ingest/vendor-api-key}
```

The engineer still sees and owns their IAM actions, ports and secrets. That is deliberate: the brief's complaint is
that teams do not understand their own infrastructure. What they **never** write or need to know is:

- account ids, CIDRs, ARNs, cluster names and registries;
- the permissions boundary, the trust policy and Pod Identity;
- the namespace and Argo CD project;
- the SSM env prefix;
- MSK, RDS and KMS identifiers. They write `msk-cluster:main` and the platform catalog resolves it
  ([`platform/catalog/`](../platform/catalog));
- the pod selector, and the platform baselines for DNS and the Pod Identity agent;
- a broker's or venue's addresses. They write `counterparty: venue-a-fix` (§6).

From that spec `svcrender` renders ([`out/render/stg/odds-ingest/`](../out/render/stg/odds-ingest)) a
ServiceAccount; an ACK `Role` with the boundary, a trust policy pinned to this cluster, namespace and service account,
and the inline policy with real ARNs; a `PodIdentityAssociation`; one `NetworkPolicy`; an `ExternalSecret`; the
`<svc>-infra` Application for those; the chart Application once Kargo has promoted a version; and, once per service,
the dev ECR repository.

## 2. Where each rule holds

A service repository is untrusted, and so is the **chart** it publishes. Each rule is held by the earliest layer that
can decide it, and again by a later layer that holds even when the earlier one is bypassed or buggy. Authoring-time
checks (a pre-commit hook or the service's own CI running `svcrender check`) give fast feedback. They never count
as enforcement, because the service team controls them.

| Rule | Render (`svcrender`, platform CI) | Admission (cluster) | Cloud (AWS) | Can render decide it? |
|---|---|---|---|---|
| **R1** own namespace and project | The registry gives the team, keyed by source repo. A spec that claims another service, or sets `project`, `namespace`, `chart` or `image`, is refused ([`checkR1`](../internal/svc/rules.go)) | The team `AppProject` allows the team namespace only, no cluster kinds, and only an **allow-list** of namespaced kinds, none of them the platform's ([`projects.yaml`](../policies/argocd/projects.yaml)). VAPs refuse any Application in `default` or an unknown project, a team Application outside its namespace, and a team project wider than that ([`application.yaml`](../policies/admission/application.yaml)) | n/a | Yes for the spec. **No for the chart**, which only the AppProject sees |
| **R2** boundary, no `*`, no IAM/STS/KMS-mgmt | Action and resource checks, case-insensitive as IAM is. **Ownership:** a grant touches only the team's own resources or ones the owner shared. No action that controls who reaches a resource (`s3:PutBucketPolicy`, or `s3:Put*`, which covers it). The boundary is always attached ([`rules.go`](../internal/svc/rules.go), [`ownership.go`](../internal/svc/ownership.go)) | A VAP requires this account's boundary, Pod Identity trust for one namespace and service account, and refuses `*`, service-wide and leading wildcards, `NotAction`, managed policies, JSON escapes and control actions. A Pod Identity association binds only to its own role, with no role chaining ([`iam-role.yaml`](../policies/admission/iam-role.yaml)) | The boundary denies IAM, STS, KMS management and the control actions. The ACK controller can only create `svc-*` roles *with* the boundary, and can never remove or edit it ([`policies/aws/`](../policies/aws)) | Yes |
| **R3** SSM under `/<env>/<team>/` | Path checked against the registry team; env prefix injected ([`checkR3`](../internal/svc/rules.go)) | The per-namespace `SecretStore` role can only read `/<env>/<team>/*` | The IAM policy of that store role | Yes |
| **R4** network shape | Rendered from intent: the selector is always this service's pods, `{vpcCidr}` and `{albSubnets}` are filled in, internet is 443-only, names its `hosts:` and excludes the VPC, and a broker or venue is a catalog `counterparty` the team is cleared for ([`checkR4`](../internal/svc/rules.go)). A platform default-deny covers every pod no service policy selects ([`default-deny.yaml`](../policies/network/default-deny.yaml)) | A VAP covers **every namespace not labelled as a platform namespace**. It allows only: one service's pods, peers that name one namespace, this cluster's exact VPC range or load-balancer subnets, the Pod Identity agent, a counterparty's exact `cidr:port` cleared for that team, or internet on 443 that excludes the VPC and records its hosts ([`networkpolicy.yaml`](../policies/admission/networkpolicy.yaml)). The AppProject forbids NetworkPolicy in charts | n/a | Yes |
| **R5** no env facts in the repo | Every scalar and every comment in the spec is scanned for account ids, ARNs, IPs, cluster and registry names ([`checkR5`](../internal/svc/rules.go)) | n/a | n/a | **Only for the spec.** The chart and the rest of the repo are out of reach. This is a convention there, not a rule |
| **R6** live in stg first | Prd is rendered from the Freight's spec commit, never from repo HEAD. Each env's chart and image come only from that env's pointers | n/a | n/a | **No.** It is a fact about time: the soak gate (Part C) decides it |
| **R7** explicit envs, prd ⇒ stg | Refused at render ([`checkR7`](../internal/svc/rules.go)) | n/a | n/a | Yes |
| **R8** no silent removal of a used permission | The spec's grants are compared with **observed usage** of the pod's current identity. A removal is refused unless re-granted or listed in `acknowledgeRemovals`. A running service with no usage data is flagged ([`checkR8`](../internal/svc/rules.go)) | n/a | IAM last-accessed data, CloudTrail and MSK logs feed the usage file | **Partly.** "Used" is runtime knowledge, so render can only be as good as the usage data |

Why render is the main gate: it is the one place that sees intent, so it can say *what to write instead*. Every
finding carries a rule, a path in the spec, the problem and an exact fix (see
[`out/render/verdicts.txt`](../out/render/verdicts.txt)).

Why admission and AWS as well: render is code the platform team writes, and it will have bugs. A hand `kubectl
apply`, a compromised bot or a bad merge into `platform-gitops` never passes through render. The later layers check
**shape**, not intent, and admission is held to be **at least as strict as render**. An adversarial pass showed it
was not: it admitted 8 of 11 hostile objects, including `s3:*` on `*` written as JSON escapes
([notes](notes.md#adversarial-pass-and-what-changed-because-of-it)). Now every rendered object must pass it, and 51
hostile objects must be refused, each by the policy meant to refuse it. That is checked offline
(`policies/admission/test.sh`, kyverno) and on a real API server ([`tests/cluster/run.sh`](../tests/cluster/run.sh),
[evidence](evidence/cluster-run.txt)).

## 3. How a change travels

```mermaid
sequenceDiagram
  participant Eng as Engineer
  participant SR as Service repo
  participant CI as platform-gitops CI
  participant WH as Kargo warehouse
  participant Stg as stg (auto-sync)
  participant Gate as soak gate
  participant Prd as prd (manual sync)
  Eng->>SR: commit spec / chart / code
  SR->>WH: new spec commit, chart version, image tag
  WH->>WH: Freight F = (spec@C, chart v, image t)
  WH->>CI: promote F to stg
  CI->>CI: svcrender stg with spec@C + pointers(F)
  CI->>Stg: bot MR, auto-merge on ACCEPT; Argo syncs
  Stg-->>Gate: deployed / degraded / recovered events for F
  Gate->>WH: F PROMOTABLE (or WAIT until T / BLOCKED because ...)
  WH->>CI: promote F to prd
  CI->>Prd: svcrender prd with the same spec@C; MR merged
  Prd->>Prd: human presses sync
```

- **The unit of promotion is the Freight:** spec commit, chart version and image tag together. Image, chart and
  infrastructure change independently, but they are tested together. Promoting them separately would let prd run a
  combination stg never ran: a new image with an old role, say. A change to any of the three makes a new Freight.
  Unchanged parts are carried along, so an image-only change still re-renders, with an identical result.
- **The revision the gate reasons about is the Freight id.** Argo CD notifications emit `deployed`, `degraded` and
  `recovered` for the Freight, and every rendered object carries `platform.example.org/spec-digest`, so the
  objects can be traced back to the spec commit.
- **prd is rendered only inside the prd promotion,** after the gate has passed, from the Freight's spec commit. The
  local `svcrender all` renders both environments side by side to show the output. It is not the delivery path.
- **prd stays human-synced,** as today. The human syncs Freight that already passed the gate, so their job is
  timing, not review.

## 4. Failure modes

**A change passes every check and is rejected downstream.** Four cases:

| Case | What happens downstream | Mitigation |
|---|---|---|
| rfq-router's chart contains a ClusterRole | The team AppProject refuses it at sync. Render never sees the chart | The Argo sync failure goes to the team's channel. Next: render the chart with `helm template` and check its kinds against the project before merge (cut from day one) |
| An S3 caller with no internet egress | S3 sits behind a VPC gateway endpoint, whose addresses are outside `{vpcCidr}`, so the NetworkPolicy drops the traffic | svcrender already warns ([settlement-writer](../out/render/verdicts.txt)) |
| The renderer emits an object the API server refuses. This happened: every prd Application had `syncPolicy: null` ([notes](notes.md#where-the-ai-assistant-was-wrong-and-what-caught-it)) | Argo CD cannot sync it | Here, [`tests/cluster/run.sh`](../tests/cluster/run.sh) applies every rendered object to a real API server with the real CRDs. In production, platform CI would server-side dry-run each MR against the target cluster, which also runs admission, before it merges |
| A bucket, SSM parameter or catalog entry that does not exist | The ExternalSecret goes `SecretSyncedError`, or the role grants nothing useful | Pods go unhealthy, which emits `degraded`, which keeps that Freight out of prd. Promotion is the safety net, not review. Next: existence checks against an inventory snapshot |

**A change removes a permission the running pod depends on (R8).** This is request 02. Leaving the shared
`data-services` role silently drops the producer's `WriteData` on `pp-settlement-events`, which nobody listed. The
renderer compares the new grants with what the pod's *current* identity was observed to use. That data comes from
IAM last-accessed, CloudTrail and MSK access logs, attributed to the pod through Pod Identity session tags. Any used
permission no longer granted is an error that names the exact grant, plus the exact acknowledgement if the removal is
intended. The role is held back while the rest renders. An acknowledged removal stays visible as a warning in the
MR. A running service with no usage data is flagged instead of passing silently, so before cutting over a shared
role, usage collection must be on for 30 days.

**The same setting defined in two places.** Every field has exactly one owner, and the other place cannot set it:

| Setting | Owner | The other place is stopped by |
|---|---|---|
| Team, namespace, project, chart name, image name | Registry (team leads) | Spec fields reserved → R1 error |
| Chart version, image tag | Kargo pointers | Spec has no such field; render reads pointers only |
| ServiceAccount, NetworkPolicy, ExternalSecret, role | svcrender | Chart gets `serviceAccount.create=false`; the AppProject forbids the rest in charts |
| Ports, egress, IAM actions, secrets | Spec | n/a |
| Replicas, resources, probes | Chart | n/a |

The remaining gap: a chart that hard-codes a port different from the spec's. The pods run, and the policy blocks the
traffic. That shows up as `degraded` in stg and never reaches prd.

## 5. Migration of the 35 running services

1. **Registry first.** One platform MR adds a line for each of the 35 services. Nothing changes in any cluster.
2. **Specs.** `svcrender import`, the first thing built after day one, drafts a spec from each service's five files.
   Until it exists, the platform team writes them, about 20 minutes each. Teams then review their own spec, which is
   the first time they see their infrastructure.
3. **Shadow render.** CI renders every migrated spec and diffs it, after normalization, against the hand-written
   objects. A `legacyNames` field in the registry makes the renderer produce the **same object names** (not built). A service is
   ready when the diff is empty, or each line is an explained tightening.
4. **Cut-over, one service per commit.** Objects get `Prune=false` in the old app, and the same-named rendered
   objects move under `<svc>-infra`, where Argo adopts them in place: no delete, no recreate. ACK *adopts* an existing
   role rather than creating one. A **shared** role has nothing to adopt: create the new role, confirm parity with
   usage data (R8), switch the Pod Identity association (pods pick up new credentials on refresh), and leave the old
   role attached to nothing for 30 days.
5. **Order:**
   - stg before prd for every service, with prd only after the stg cut-over has soaked.
   - Within that, ingestion first as the canary team (fewest cross-namespace edges), then services without IAM, then
     single-team services.
   - Services with cross-namespace traffic go together with their peers.
   - Shared-role services (settlement-writer and the other `data-services` users) go last.
6. **Deleting the old files is safe when all of these hold:**
   - the shadow diff is empty;
   - `argocd app resources` shows the old app owning nothing;
   - 7 days pass with no drift and no new AccessDenied in CloudTrail for the service role;
   - NetworkPolicy deny logs are flat;
   - for shared roles, IAM last-used shows no sessions for 30 days before the role is deleted.

## 6. What a trading venue changes

The brief is generic, but the services are not: rfq-router holds venue credentials, and settlement-writer signs
settlement reports. Five things follow.

**Brokers and venues are catalog entries (built).** A FIX session runs to fixed addresses, usually over a
cross-connect or VPN, and both sides allow-list them. So "no literal CIDRs" cannot mean "no addresses": it means none
in a service repository.
- A spec writes `- counterparty: venue-a-fix, ports: [9880]`. The catalog holds each environment's endpoints, ports,
  and the teams cleared to connect, since a session with a partner is a contract, not a self-service choice
  ([`platform/catalog/`](../platform/catalog), [`checkCounterparties`](../internal/svc/rules.go)).
- The renderer writes the exact address blocks. Admission accepts a literal address only as a `cidr:port` pair that
  the parameters clear for that namespace's team ([`networkpolicy.yaml`](../policies/admission/networkpolicy.yaml)).
- A partner's address written literally is refused, and the fix names its catalog entry.
- On a real cluster, a stand-in gateway on the venue's range is reached by rfq-router on 9880. It is not reached on
  another port, or from a team that is not cleared ([evidence](evidence/cluster-run.txt)).
- The other direction belongs to the platform too: partners allow-list *our* addresses, so session traffic leaves
  through platform-owned NAT with fixed addresses per environment. Changing them is a change coordinated with the
  partner, never a spec field.

**Sessions are stateful, so deploys need a shape.** A FIX session is one logon per sender and target identity,
with sequence numbers that carry across reconnects. Two pods cannot both hold it, and a rolling update mid-session
forces a reconnect and sequence recovery. Session-holding services run one active instance with a standby, and roll at the venue's session
boundary. That is the chart's concern, but it is why the next two points matter.

**Rollback never waits for the soak gate.** The gate decides whether *new* Freight is promotable. Going back to the
Freight that was last healthy in prd is a different act, and as written the gate would BLOCK it, because that Freight
is no longer live in stg. Rollback should be one step: immediate, audited and paged. It is never gated on stg.

**Change windows.** Outside a rollback, no prd sync during market hours or a settlement run. The gate already takes
its policy as a value, so a calendar is one more input to it. I have left it unbuilt on purpose.

**An audit trail, without a ticket queue.** Every prd change already has a chain:
- the spec commit and its author;
- the bot MR with the rendered diff;
- the gate's verdict with its reasons;
- the person who synced.

Keeping the verdict with the promotion gives a partner's or auditor's due diligence ("who changed what, who approved
it, what was it tested on") an answer for every change. The one rule worth adding is that the person who syncs prd
is not the spec's author.

On latency: order-path egress uses counterparty entries (addresses, enforced in the kernel), not FQDN rules, which
add a DNS proxy. Any network policy change on the order path is measured at p99 before and after.

## 7. What I cut from day one, and why

- **Chart introspection** (`helm template` against the AppProject). The AppProject already refuses at sync; this only
  makes the error earlier. It's the first thing to build next.
- **`svcrender import`.** Migration can start by hand.
- **Inventory checks** (does the bucket or parameter exist?). The registry already refuses a resource nobody is
  recorded as owning. Promotion catches the rest through `degraded`.
- **A usage collector.** R8 reads a file, and the collector is a pipeline from CloudTrail, IAM and MSK logs to it.
  Shared-role migration waits on this; nothing else does.
- **An exceptions workflow, a UI or Backstage, cost reporting, multi-region.**

## 8. Unknowns I would verify first

Settled since the first draft, by documentation or source, not by an AWS account (sources in
[notes](notes.md#things-verified-by-documentation-not-by-running-them)):
- The VPC CNI's policy agent matches `ipBlock` by address, so pods inside `{vpcCidr}` match. Hence `{albSubnets}`
  and the default-deny baseline.
- The agent auto-allows only the node's own address, so the Pod Identity baseline (`169.254.170.23:80`) is needed.
- Pod Identity session tags can be conditioned on in the trust policy.

Still open:
- Whether S3 and STS use gateway or interface endpoints. This flips the S3 warning.
- How MSK ACLs and IAM auth interact on the existing clusters.
- The ALB target type, and the real internal load-balancer subnets. `{albSubnets}` assumes IP targets; with
  instance targets, traffic arrives from node addresses.
- Who owns which bucket and topic. The owners in the registry are invented.
- Who owns cross-team SSM values today (rfq-router reads under `state`).
- The existing role and object names, which migration needs to keep.

## 9. What I would build instead

The design above answers the brief as asked. The adversarial pass says something about the brief itself. Most of the
holes came from **rules written in the vocabulary of today's files**:
- R2 limits how wide a grant is, but not whose resource it is on.
- R4 speaks in CIDRs, which on the VPC CNI also mean pods.
- R6 counts minutes in an environment that request 04 says "costs money and nobody uses it".

Every rule I enforce before the cluster is convenience. Only the chokepoints a team cannot route around hold, and
they hold best when the rule is stated **once, there**.

| Concern | Here | Instead |
|---|---|---|
| Who a service is | Registry entry keyed by repository | The namespace and service account. Pod Identity puts both in every AWS session as tags |
| IAM | A rendered role per service, an engineer-written policy | **One static policy for every service role, keyed on those tags** ([`service-abac-policy.stg.json`](../alternative/iam/service-abac-policy.stg.json)): own team's parameters, buckets, topics and keys; own database user and consumer groups. Nobody writes IAM, and R2 and R3 hold in AWS itself |
| Cross-team access | Owner shares in the registry | The owner's own resource policy names the consumer ([`owner-share-bucket-policy.stg.json`](../alternative/iam/owner-share-bucket-policy.stg.json)) |
| Infra intent | A spec, rendered by platform CI, via bot MR | A small values block in the service's own chart, rendered by a platform **library chart**. A Freight is then (chart, image), what Kargo already promotes, so infra soaks with the code |
| Rules | Go, CEL and IAM, three copies | **The admission policies are the rule set.** Service CI runs them offline (kyverno) for feedback; the cluster enforces the same file |
| Internet egress | `0.0.0.0/0:443` plus a host list | **By name** ([`fqdn-egress.yaml`](../alternative/network/fqdn-egress.yaml)): the named host on 443, nothing else |
| R8 | Usage data compared with the spec | An update that drops a rule or an action is refused unless it says why ([`acknowledge-removal.yaml`](../alternative/admission/acknowledge-removal.yaml)), plus Access Analyzer's unused-access findings |
| R6 | Wall-clock soak in a quiet stg | The soak gate as one input to a Kargo verification on real signals (error rate, latency, a synthetic check), then a canary in prd |
| ECR | One rendered repository per service | Create-on-push from a repository template, and replication instead of a prd cache of stg |

**What is proven here.**
- On real clusters ([`tests/cluster/run.sh`](../tests/cluster/run.sh), [evidence](evidence/cluster-run.txt)):
  - the R8 admission policy refuses an unacknowledged removal and admits the acknowledged one, for a NetworkPolicy
    and for an IAM role;
  - an FQDN policy on Cilium reaches the named host and nothing else on 443.
- **Not executed:** the ABAC policy (no AWS account here). Each of its mechanisms is documented
  ([`alternative/README.md`](../alternative/README.md)).

**What it costs.**
- Cilium (or an egress proxy) is a dataplane change on two clusters, the largest risk. Do ABAC and admission first.
- ABAC needs naming conventions (team-prefixed buckets and topics, `team`-tagged keys, database users named after
  service accounts). Renaming buckets is migration work.
- Policy errors in CEL are terser than the renderer's fixes, so every validation needs a message that names the fix.
- It changes the rules: "engineers write IAM" goes away. I would propose that as an amendment, not assume it.
