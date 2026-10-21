# Working notes

Rough, in the order things happened. The decisions I expect to be asked about are marked **(decision)**.

## Reading the brief

- The four requests are tests of the trust boundary:
  - **01** is the happy path.
  - **02** hides an R8 trap: the shared role's Kafka write isn't mentioned.
  - **03** breaks almost every rule.
  - **04** breaks R7 and R5, and has a subtle network mistake (an ALB is not a pod in `kube-system`).
- Cross-request finding: 02 opens 9090 to `distribution` for trader-ui, but 04 (trader-ui) never asks for the egress
  to match. Under default-deny, both sides must declare it. The tool reports this as a fleet warning.
- The brief's inputs disagree with each other. Requests 01 and 03 call odds-ingest and rfq-router *new*, but the event
  stream shows both deployed. Part B follows the requests (no Kargo pointers yet for those two). Part C follows the
  events. Where I invented Part B state, I kept it consistent with the events (trader-ui's stg pointer is r21,
  because the stream says r22 never ran in stg).
- `chartRegistry` is dev ECR in *both* env configs. Charts never go through the stg→prd pull-through chain, so R6 for
  charts cannot lean on ECR. It has to be the Freight pin.

## Part C first (decision)

- It is graded on its own and extended live, and its verdicts could be worked out by hand before writing code. Doing
  it first also gave the design its R6 answer.
- I worked out all 11 verdicts by hand first, then wrote the code until it agreed (`out/promotion/verdicts.txt`).
- **(decision) Data model.** Events are folded into *stints*: uninterrupted spans in which one revision was live and
  healthy. The rule then reads like the brief: "the stint containing `now` is at least `soak` long".
  - Interruptions end a stint.
  - A recovery or redeploy starts a new one.
  - Healthy time is never added up across stints.
- **Input defects and what the gate does:**

| Defect | Line | Handling | Why |
|---|---|---|---|
| Exact duplicate (`e-003`) | 19 | Counted once, reported | At-least-once delivery makes this expected |
| Same id, different content (`e-022` degraded and recovered) | 17 | **Both kept**, reported | De-duplicating by id would drop the recovery and block r4 forever. Two different facts under one id is a producer bug; dropping either one is a guess |
| Truncated line (`e-050`) | 15 | **(decision) Fail closed, scoped.** The fields that survive (id, `at` 11:05, env stg, service prefix `settlem`) block only the queries the event could affect, and the reason says what the verdict would otherwise be | The gate exists to be safe. Blocking everything for one bad line would teach people to ignore it. Settlement-writer at 12:00 is BLOCKED ("would otherwise be WAIT until 12:45"); at 10:40 it is unaffected |
| `+01:00` offset (`e-041`) | 12 | Normalized to UTC, reported | 10:30+01:00 is 09:30Z. That makes book-builder r9 PROMOTABLE at 12:00, and puts r8's `degraded` (10:00Z) after r9 went live, so it is about a revision that is no longer live |
| Lines out of time order | – | Sorted by time; ties go to the pessimistic state | Stated in the contract, so not a defect |
| prd events in the stream | – | Ignored for soak; used only to audit past promotions | |
| trader-ui r22 deployed to prd, never in stg | 7 | Reported as a past promotion the gate would have refused | An R6 breach already in history |

- **Semantics I chose:**
  - An event at `now` counts; an event after `now` is unknown.
  - Exactly `soakMinutes` is enough (`>=`).
  - A redeploy of the healthy live revision does not restart the clock.
  - A `recovered` with no `degraded` before it does nothing.
  - A `degraded` or `recovered` about a revision that is not live is recorded but cannot interrupt or resume anything.
- **Testing:** there is one test per trap, and I mutation-checked them. I broke the gate seven naive ways: no env
  filter, using future events, de-duplicating by id, applying `degraded` to whatever revision is live, a strict `>`
  boundary, ignoring a redeploy of a degraded revision, and ignoring tainted lines. The tests caught every one.

## Part B

- **(decision) The core is the renderer with the rules, not a CLI or a UI.** If the renderer is wrong, every layer
  after it is cleaning up.
- **(decision) Engineers write IAM actions, not capabilities like "kafka-producer".** The brief's complaint is that
  teams do not understand their own infrastructure. They keep the actions; what they lose is ARNs, accounts and the
  boundary. Resources are platform references (`msk-topic:main/odds.raw`) resolved from a catalog.
- **(decision) Two kinds of error.** A spec-level error (R1, R7, schema) refuses everything. An error in a list item
  refuses that item, and the rest renders as **PARTIAL**, so the engineer sees what they would get.
  - R8 refuses the whole role and leaves the rest.
  - Nothing partial is ever applied on its own. The MR only merges on ACCEPT.
- **(decision) Who the service is comes from the repository, not from the spec.** The first version looked the
  registry up by the spec's own `service:` field. A second AI review pass caught that rfq-router's repository could then claim
  `service: settlement-writer` and get that service's namespace, role and service account. It is now keyed by the
  CI job's project path, and chart and image moved into the registry. There's a test for exactly that impersonation.
- **(decision) R8 needs data the brief does not provide.** I made `platform/observed/` a stand-in for usage collected
  from IAM last-accessed, CloudTrail and MSK logs. Without usage data, a running service gets a warning rather than a
  silent pass.
- **(decision) A fleet check across specs** for one-sided network rules. Policies are enforced at both ends, so one
  side alone admits nothing.
- **(decision) Brokers and venues are catalog entries, not CIDRs.** A FIX session goes to fixed, allow-listed
  addresses, so refusing every literal address would leave rfq-router no way to reach its venue. A spec names a
  `counterparty`. The catalog holds its addresses, ports and the teams cleared for it, and admission gets the same
  facts as `cidr:port` pairs per team (a test keeps the two equal). Clearing a team is a platform change on purpose: a
  partner session is a contract ([design §6](design.md#6-what-a-trading-venue-changes)).
- **Verdicts:** see `out/render/verdicts.txt`. The fixed specs in `specs/fixed/` follow each fix literally and come
  out clean; a test keeps it that way.

## Things I invented (the brief gives none of them)

- **Catalog ids:** MSK cluster names and UUIDs, the RDS resource id, the KMS key id, the region (`platform/catalog/`).
- **Observed usage** for settlement-writer and trader-ui (`platform/observed/`). Topic and group names other than
  `pp-settlement-events` are guesses: `settlement-instructions`, the `settlement-writer` group, and `odds.raw` for
  odds-ingest.
- **Kargo pointer versions** for the existing services (`platform/pointers/`). New services have none until their
  first promotion.
- **The registry** (`platform/registry.yaml`) and its per-team sharding.
- **Resource owners** in the registry: which team owns each bucket, topic prefix and key. The brief says who uses
  them, not who owns them. `settlement-instructions` and `pp-settlement-events` are given to state.
- **Load-balancer subnets** (`albSubnets` in `platform/catalog/`) and the auth provider's host
  (`login.auth-provider.example` in the fixed trader-ui).
- **Buckets and callers in the fixed rfq-router:** `rfq-audit`, and callers in its own namespace.
- **The counterparty `venue-a-fix`** (`platform/catalog/`): its address blocks (documentation ranges, RFC 5737), the
  port `9880`, the cleared team, and that rfq-router opens a FIX session to it.
- **Assumptions, still unverified:**
  - S3 is reached through a **gateway** endpoint (drives the S3 warning).
  - The `ack-services` and `services-infra` names.
- **Assumptions since settled:**
  - The ACK resource fields (`Role.spec.inlinePolicies`, `PodIdentityAssociation.spec.roleARN`, …): every rendered
    object passes strict field validation against the real ACK CRDs ([cluster run](evidence/cluster-run.txt)).
  - The Pod Identity agent's address and the session-tag trust conditions, by AWS documentation (below).

## Where the AI assistant was wrong, and what caught it

I worked with an AI assistant throughout, and had its work reviewed by a second, separate AI review pass. These are
the cases where the assistant's output was wrong, and what caught each one:

1. **R5 skipped resource references.** To avoid a duplicate finding for ARNs, the scan skipped `permissions[].resources`
   entirely, so a hard-coded `trading-platform-stg-…` bucket passed. A test I wrote for that case failed, and now
   only ARNs are skipped there.
2. **An IP address went into a fixed spec.** The assistant put "203.0.113.40 today" in trader-ui's reason text. R5
   refused it: the tool caught its own author.
3. **A false green from kyverno.** `kyverno apply --resource <dir>` does not walk directories, and reported
   `pass: 0, fail: 0` as success. Caught by reading the numbers, not the exit code. The script now passes files
   explicitly and treats "nothing checked" as a failure.
4. **The admission policies had no bindings.** A ValidatingAdmissionPolicy without a binding does nothing in a real
   cluster, and the offline CLI ran them anyway, so tests were green for policies that would never fire. Caught by
   the second AI review pass. With bindings, the CLI then applied every policy to every kind until the bindings repeated the policies'
   match rules.
5. **The registry was keyed by the spec's own claim** (see Part B above). It was the most serious one, caught by the second AI review pass.
6. **Dependencies weren't vendored** although the plan said so. Caught by the second AI review pass; it now builds
   with `GOPROXY=off`.
7. **CI would have failed on its first run.** `gofmt -l .` also checks `vendor/`, where yaml.v3 predates current
   gofmt. Local checks had filtered vendor out, so they never showed it. Caught by the second AI review pass.
8. **Parts B and C disagreed about trader-ui.** The invented stg pointer said r22, which the event stream says never
   ran in stg. Caught by the second AI review pass.
9. **The admission layer was a weaker copy of the renderer,** so "defence in depth" was mostly one layer. Green offline
   tests hid it, because the same author wrote the policies and the fixtures. An adversarial pass caught it (above).
10. **The never-grantable check was case-sensitive.** `IAM:PassRole` was refused only by an unrelated check, with a
    wrong message. Caught by the adversarial pass.
11. **Two S3 actions that don't exist.** The first list of resource-control actions had `DeleteBucketPublicAccessBlock`
    and `DeleteBucketOwnershipControls`. Caught by checking every name against the Service Authorization Reference.
12. **Admission refused what the renderer allows.** The first Kafka pattern also matched `kafka-cluster:AlterGroup`.
    Caught because the admission tests run every rendered object, not only hand-written fixtures.
13. **A test object that tested nothing.** `json.dumps` doubled the backslash, so the "escaped star" decoded to a
    literal `*`. Caught by decoding the fixture before trusting it.
14. **A reserved word in CEL.** A variable named `namespace` passed kyverno and was refused by the API server. Caught
    by the real-cluster run.
15. **Every prd Application was invalid.** prd has no auto-sync, and the renderer wrote `syncPolicy: {}`. It reaches the
    API server as `null`, which the Application CRD refuses. Golden files, kyverno and `go test` all passed it.
    Caught by applying the prd objects to a real API server with the real Argo CD CRD; prd now has no `syncPolicy`,
    and `TestOnlyStgSyncsItself` holds it.
16. **Smaller things the tests caught:**
   - a test expected WAIT where 180 minutes was already PROMOTABLE (the test was wrong);
   - fixture ordering put appended permissions inside `secrets`;
   - golden files embedded a temp path;
   - a shell loop relied on word-splitting that zsh does not do, which produced garbled CRD stubs.

## Adversarial pass, and what changed because of it

With everything green, I attacked my own solution:
- 15 hostile specs against the renderer;
- 11 hostile objects against the admission policies;
- 7 hostile event streams against the gate.

Every attack is now a test: [`internal/svc/adversarial_test.go`](../internal/svc/adversarial_test.go), the last block of
[`promotion/gate_test.go`](../promotion/gate_test.go) and [`policies/admission/tests/bad.yaml`](../policies/admission/tests/bad.yaml).

**The renderer** accepted 9 of the 15 attacks and refused 4 more only by accident:

| Attack | Before | Now |
|---|---|---|
| `IAM:PassRole`, `Sts:AssumeRole`, `KMS:CreateGrant`, `" iam:PassRole"` | Refused only because no resource matched the service, with a wrong message. The never-grantable check was case-sensitive, and IAM is not ([IAM docs](https://docs.aws.amazon.com/IAM/latest/UserGuide/reference_policies_elements_action.html)) | Lowercased before every check. An action must be ASCII `service:Action` |
| `s3:Put*` (R2-legal) | Accepted. It covers `PutBucketPolicy` and `PutBucketAcl` | Refused when a pattern can match any action that changes who can reach a resource. The boundary denies those actions too |
| Another team's bucket, `msk-topic:main/*`, `rds-dbuser:settlements/*` | Accepted. Neither R2 nor the renderer knew whose a resource is | **(decision) Ownership.** The registry records each resource's owner team. Another team's resource needs the owner to share it, in the owner's own registry shard. Consumer groups and DB users must be the service's own |
| Acknowledging a removal with `resource: "*"` or `reason: x` | Accepted, which switched R8 off | One exact action on one exact resource, with a reason of at least two words |
| Account id in a YAML comment | Accepted | R5 scans comments too |
| Internet egress with `reason: "."` | Accepted | **(decision) `hosts:` is required on internet egress.** It names what the rule is for. A NetworkPolicy can't enforce it, but review can read it, it is recorded on the object, and an FQDN policy enforces exactly that list ([`alternative/network/`](../alternative/network)) |
| Egress `namespace: "*"` | Rendered a selector that matched nothing | Refused: a namespace must be a DNS label |
| Ingress from `{vpcCidr}` for the ALB | Accepted. On the VPC CNI, pods have VPC addresses, and the policy agent matches `ipBlock` by address, so this also admits every pod | **(decision) A `{albSubnets}` token** (load-balancer subnets from the catalog). `{vpcCidr}` ingress now gets a warning. Separately, a platform default-deny in every team namespace means a VPC-wide egress rule reaches only pods whose own policy admits it |

**Admission** admitted 8 of 11 hostile objects and never evaluated 2: it covered only namespaces and Applications that
carried a label. The worst was an ACK Role whose inline policy wrote `s3:*` on `*` as `"s3:*"` and `"*"`. The
regex looked for a literal `*`. JSON decodes the escape, and the boundary allows S3.

- **(decision) Admission is at least as strict as the renderer.** It now:
  - covers every namespace that is not explicitly labelled as a platform namespace;
  - takes per-cluster facts as parameters;
  - refuses any backslash in a policy document. This is what makes text checks sound, since CEL cannot parse JSON.
    Whether IAM decodes `\u` escapes is not documented, so this refusal makes the answer irrelevant;
  - refuses `NotAction`, attached managed policies, role chaining through `targetRoleARN`, and disabled session
    tags;
  - binds each Pod Identity association only to its own `svc-<namespace>-<service account>` role.
- **(decision) Team AppProjects list the kinds they allow, instead of the kinds they refuse.** A CRD installed later is
  refused until someone decides it is safe.

**The gate** dropped or misread six inputs:

| Input | Before | Now |
|---|---|---|
| An event for env `"STG"` | Dropped silently | `unknown-env` defect. Fails closed for that service |
| Two revisions deployed at one instant | Decided by id string order (`"e-10" < "e-9"`) | `same-instant` defect. Blocks until the next deploy decides |
| A line over 64 KiB | The whole gate failed | `unreadable` defect for that line only (limit 1 MiB) |
| A duplicate JSON key | Last one won, silently | `invalid` defect. Fails closed |
| An unknown field such as `"healthy": false` | Ignored, silently | `unknown-field` defect, reported. `kind` still decides |
| `degraded` 1 s before its own deploy (clock skew) | Ignored, so the revision looked healthy | `health-before-deploy` defect. Applied from the deploy |

One thing is left as the contract says: a re-sync `deployed` after `degraded` counts as a recovery.
`TestResyncAfterDegradedRestartsTheClock` pins that reading.

**A real cluster** ([`tests/cluster/run.sh`](../tests/cluster/run.sh), evidence in
[`docs/evidence/cluster-run.txt`](evidence/cluster-run.txt)) found what the offline CLI could not. The first run failed:
`namespace` is a reserved word in CEL, so a policy that kyverno had evaluated happily was invalid on a real API
server. A later run found that every prd Application was invalid (`syncPolicy: null`, item 15 above); that run's
transcript is kept in [`docs/evidence/cluster-run-before-syncpolicy-fix.txt`](evidence/cluster-run-before-syncpolicy-fix.txt).
The transcript's `Warning: store ssm isn't currently maintained` is expected. `ssm` is the store's name, and the test
stores use External Secrets' fake provider, which v2.13.0 marks as not maintained
(`providers/v1/fake/fake.go`, `MaintenanceStatus`).

## Things verified by documentation, not by running them

There is no AWS account here, so these are cited, not executed:
- IAM action names are case-insensitive ([IAM](https://docs.aws.amazon.com/IAM/latest/UserGuide/reference_policies_elements_action.html)).
- `Allow` with `NotAction` grants everything not listed ([IAM](https://docs.aws.amazon.com/IAM/latest/UserGuide/reference_policies_elements_notaction.html)).
- Pod Identity session tags, and their use as `${aws:PrincipalTag/...}` ([EKS](https://docs.aws.amazon.com/eks/latest/userguide/pod-id-abac.html)).
- The Pod Identity agent listens on `169.254.170.23:80` (IPv6 `fd00:ec2::23`) ([EKS](https://docs.aws.amazon.com/eks/latest/userguide/pod-id-agent-setup.html)).
  The VPC CNI policy agent allows only the node's own address by default, so the egress baseline is needed. This
  comes from the agent's source ([aws-network-policy-agent](https://github.com/aws/aws-network-policy-agent)).
- The VPC CNI matches `ipBlock` against any address, pods included. This also comes from the controller and agent
  source, not from a doc ([network policy controller](https://github.com/aws/amazon-network-policy-controller-k8s)).
  Kubernetes leaves it implementation-defined ([kubernetes.io](https://kubernetes.io/docs/concepts/services-networking/network-policies/#behavior-of-ipblock-selectors)).
- S3 through a gateway endpoint uses S3's public addresses ([VPC](https://docs.aws.amazon.com/vpc/latest/privatelink/vpc-endpoints-s3.html)).
- S3 has no `DeleteBucketPublicAccessBlock` or `DeleteBucketOwnershipControls` IAM action: the Put action authorises
  the delete ([S3 API](https://docs.aws.amazon.com/AmazonS3/latest/API/API_DeletePublicAccessBlock.html)).
- The 19 `kafka-cluster:` actions ([MSK](https://docs.aws.amazon.com/msk/latest/developerguide/kafka-actions.html)).
- ValidatingAdmissionPolicy is GA since Kubernetes 1.30, so the admission policies need EKS 1.30 or later
  ([kubernetes.io](https://kubernetes.io/docs/reference/access-authn-authz/validating-admission-policy/)). They were run
  on 1.37.

## Open questions I would ask the team

- Do MSK clients use IAM auth only, or IAM plus ACLs? That decides whether R8 for Kafka needs MSK logs.
- How are the existing role names formed? Migration needs them for adoption.
- What are the internal load balancers' subnets, and do they use IP targets? `{albSubnets}` uses invented ranges.
- Who owns `/<env>/state/rfq-router/venue-credentials` today, and why is it under `state`?
