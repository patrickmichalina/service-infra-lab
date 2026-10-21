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

The platform team stops writing service files. It owns the renderer, the rules, the catalog of shared resources and
the exceptions.

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
  - {cidr: 0.0.0.0/0, ports: [443], reason: vendor odds feed feed.vendor-example.com}
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
- the pod selector, and the platform baselines for DNS and the Pod Identity agent.

From that spec `svcrender` renders the following ([`out/render/stg/odds-ingest/`](../out/render/stg/odds-ingest)):
- a ServiceAccount;
- the IAM role as an ACK `Role`, with the boundary, a trust policy pinned to this cluster, namespace and service
  account, and the inline policy with real ARNs;
- a `PodIdentityAssociation`;
- one `NetworkPolicy`;
- an `ExternalSecret`;
- an `<svc>-infra` Application for those objects;
- the chart Application, once Kargo has promoted a version;
- the dev ECR repository, once per service.

## 2. Where each rule holds

A service repository is untrusted, and so is the **chart** it publishes. Each rule is held by the earliest layer that
can decide it, and again by a later layer that holds even when the earlier one is bypassed or buggy. Authoring-time
checks (a pre-commit hook or the service's own CI running `svcrender check`) give fast feedback. They never count
as enforcement, because the service team controls them.

| Rule | Render (`svcrender`, platform CI) | Admission (cluster) | Cloud (AWS) | Can render decide it? |
|---|---|---|---|---|
| **R1** own namespace and project | The registry gives the team, keyed by source repo. A spec that claims another service, or sets `project`, `namespace`, `chart` or `image`, is refused ([`checkR1`](../internal/svc/rules.go)) | The team `AppProject` allows the team namespace only, no cluster kinds, and none of the platform's kinds ([`projects.yaml`](../policies/argocd/projects.yaml)). A VAP refuses team-labelled Applications in another project or namespace ([`application.yaml`](../policies/admission/application.yaml)) | n/a | Yes for the spec. **No for the chart**, which only the AppProject sees |
| **R2** boundary, no `*`, no IAM/STS/KMS-mgmt | Action and resource checks; the boundary is always attached ([`checkR2`](../internal/svc/rules.go)) | A VAP requires the boundary and Pod Identity trust, and refuses `"*"` and `svc:*` ([`iam-role.yaml`](../policies/admission/iam-role.yaml)) | The boundary denies IAM, STS and KMS management. The ACK controller can only create `svc-*` roles *with* the boundary, and can never remove or edit it ([`policies/aws/`](../policies/aws)) | Yes |
| **R3** SSM under `/<env>/<team>/` | Path checked against the registry team; env prefix injected ([`checkR3`](../internal/svc/rules.go)) | The per-namespace `SecretStore` role can only read `/<env>/<team>/*` | The IAM policy of that store role | Yes |
| **R4** network shape | Rendered from intent: the selector is always this service's pods, `{vpcCidr}` is filled in, internet is 443-only with a reason and excludes the VPC ([`checkR4`](../internal/svc/rules.go)) | A VAP refuses empty selectors, unnamed namespaces, internet on other ports, and literal external CIDRs ([`networkpolicy.yaml`](../policies/admission/networkpolicy.yaml)). The AppProject forbids NetworkPolicy in charts | n/a | Yes |
| **R5** no env facts in the repo | Every scalar in the spec is scanned for account ids, ARNs, IPs, cluster and registry names ([`checkR5`](../internal/svc/rules.go)) | n/a | n/a | **Only for the spec.** The chart and the rest of the repo are out of reach. This is a convention there, not a rule |
| **R6** live in stg first | Prd is rendered from the Freight's spec commit, never from repo HEAD. Each env's chart and image come only from that env's pointers | n/a | n/a | **No.** It is a fact about time: the soak gate (Part C) decides it |
| **R7** explicit envs, prd ⇒ stg | Refused at render ([`checkR7`](../internal/svc/rules.go)) | n/a | n/a | Yes |
| **R8** no silent removal of a used permission | The spec's grants are compared with **observed usage** of the pod's current identity. A removal is refused unless re-granted or listed in `acknowledgeRemovals`. A running service with no usage data is flagged ([`checkR8`](../internal/svc/rules.go)) | n/a | IAM last-accessed data, CloudTrail and MSK logs feed the usage file | **Partly.** "Used" is runtime knowledge, so render can only be as good as the usage data |

Why render is the main gate: it is the one place that sees intent, so it can say *what to write instead*. Every
finding carries a rule, a path in the spec, the problem and an exact fix (see
[`out/render/verdicts.txt`](../out/render/verdicts.txt)).

Why admission and AWS as well: render is code the platform team writes, and it will have bugs. A hand `kubectl
apply` or a chart smuggling a `NetworkPolicy` never passes through render. The later layers check **shape**, not
intent. That is why they can be small, and why each one is tested against every object the renderer emits
(`policies/admission/test.sh` runs them offline).

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

**A change passes every check and is rejected downstream.** Three cases:

| Case | What happens downstream | Mitigation |
|---|---|---|
| rfq-router's chart contains a ClusterRole | The team AppProject refuses it at sync. Render never sees the chart | The Argo sync failure goes to the team's channel. Next: render the chart with `helm template` and check its kinds against the project before merge (cut from day one) |
| An S3 caller with no internet egress | S3 sits behind a VPC gateway endpoint, whose addresses are outside `{vpcCidr}`, so the NetworkPolicy drops the traffic | svcrender already warns ([settlement-writer](../out/render/verdicts.txt)) |
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

**Also designed for:**
- **Renderer bug:** golden files make every output change a reviewed diff, and admission plus AWS hold the shape.
- **Bad event data:** the gate fails closed, and only for what the defect can affect.
- **Registry abuse:** a team cannot register another team's repository, because the shard is per team under
  CODEOWNERS.

## 5. Migration of the 35 running services

1. **Registry first.** One platform MR adds a line for each of the 35 services. Nothing changes in any cluster.
2. **Specs.** `svcrender import`, the first thing built after day one, drafts a spec from each service's five files.
   Until it exists, the platform team writes them, about 20 minutes each. Teams then review their own spec, which is
   the first time they see their infrastructure.
3. **Shadow render.** CI renders every migrated spec and diffs it, after normalization, against the hand-written
   objects. A `legacyNames` field in the registry makes the renderer produce the **same object names** (not built). A service is
   ready when the diff is empty, or each line is an explained tightening.
4. **Cut-over, one service per commit.** Objects get `Prune=false` in the old app. The rendered objects with
   identical names move under `<svc>-infra`, and Argo adopts them in place. There is no delete and no recreate, so
   nothing is down.
   - IAM is the exception, because ACK must *adopt* the existing role (adoption annotation) rather than create one.
   - For a **shared** role there is nothing to adopt. Create the new role, let usage data confirm parity (R8), switch
     the Pod Identity association (pods pick up new credentials on refresh), and keep the old role attached to
     nothing for 30 days.
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

## 6. What the platform team still does

- Owns `svcrender`, the rules, the catalog, admission, the AWS backstops, Kargo and the gate.
- Adds catalog entries when a new shared resource appears.
- Handles exceptions that are not self-service by design: cross-account access (rfq-router's `sts:AssumeRole`),
  cluster-scoped kinds, new platform namespaces.
- Reviews nothing routine.

## 7. What I cut from day one, and why

- **Chart introspection** (`helm template` against the AppProject). The AppProject already refuses at sync; this only
  makes the error earlier. It's the first thing to build next.
- **`svcrender import`.** Migration can start by hand.
- **Inventory checks** (does the bucket, parameter or catalog entry exist?). Promotion catches these through
  `degraded`.
- **A usage collector.** R8 reads a file, and the collector is a pipeline from CloudTrail, IAM and MSK logs to it.
  Shared-role migration waits on this; nothing else does.
- **An exceptions workflow, a UI or Backstage, cost reporting, multi-region.**
- **Resource ownership.** Any team can name any bucket today. A complete version tags buckets with an owner team and
  refuses grants to other teams' resources unless the owner lists the grantee.

## 8. Unknowns I would verify first

- The CNI and NetworkPolicy engine. Does default-deny egress need the Pod Identity agent baseline (`169.254.170.23`)?
- Whether S3 and STS use gateway or interface endpoints. This flips the S3 warning.
- How MSK ACLs and IAM auth interact on the existing clusters.
- The ALB target type. With the VPC CNI, pod IPs are VPC addresses, so `{vpcCidr}` ingress for the ALB (fixed
  trader-ui) also admits pods. A platform token for the load-balancer subnets would be tighter.
- Whether Pod Identity session tags (`kubernetes-namespace`, `kubernetes-service-account`) can be conditioned on in
  the trust policy as rendered.
- Who owns cross-team SSM values today (rfq-router reads under `state`).
- The existing role and object names, which migration needs to keep.
