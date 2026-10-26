---
marp: true
title: Self-service service infrastructure
paginate: true
---

# Taking the platform team out of the path

Routine change: four days and two people today. Minutes and zero people, with R1–R8 still holding.

- Architecture: the design (`docs/design.md`)
- Renderer: `svcrender`, the rules and the renderer
- Soak gate: the soak gate

---

## The shape

1. The team writes **a spec** in its own repo, and **one registry line** in its team's shard of `platform-gitops`.
2. **Platform CI** reads the spec (untrusted) by repository, and `svcrender` checks and renders the five objects.
3. A bot MR **auto-merges on ACCEPT**, and Argo CD applies it.
4. **Admission** and **AWS** refuse anything that should not exist, whoever applies it.
5. **Kargo** promotes a Freight (spec commit + chart + image) to prd only after the **soak gate** passes. A human
   still syncs prd.

The platform team owns the renderer, rules, catalog and exceptions, and reviews nothing routine.

---

## What the engineer writes (request 01)

```yaml
service: odds-ingest
team: ingestion
environments: [stg, prd]
ingress:  [{namespace: monitoring, ports: [8080], reason: Prometheus}]
egress:
  - {cidr: 0.0.0.0/0, ports: [443], hosts: [feed.vendor-example.com], reason: vendor feed}
  - {cidr: "{vpcCidr}", ports: [9098], reason: MSK}
permissions:
  - {actions: [s3:PutObject], resources: ["s3-object:trading-platform-{env}-odds-snapshots/*"]}
  - {actions: [kafka-cluster:Connect], resources: ["msk-cluster:main"]}
secrets: [{key: vendor-api-key, ssm: /ingestion/odds-ingest/vendor-api-key}]
```

They never write accounts, ARNs, CIDRs, the boundary, the trust policy, the namespace, the project or the SSM env.

---

## The trust boundary: where each rule holds

| | Render | Admission | AWS |
|---|---|---|---|
| R1 namespace/project | registry keyed by *repo* | AppProject allow-list + VAP | – |
| R2 IAM | action/resource checks, **ownership** | VAP: boundary, no `*`, no escapes, no chaining | boundary; controller can't create without it |
| R3 SSM | path vs team, env injected | store scoped to team | store role |
| R4 network | rendered from intent + default-deny | VAP: shape, exact VPC, every namespace | – |
| R5 env facts | scan every value and comment | – | – |
| R6 stg first | prd only from the promoted spec | – | – (the **gate** decides) |
| R7 envs | refused | – | – |
| R8 used permissions | grants vs **observed usage** | – | usage data source |

Render can say *what to write instead*. Admission is held to be at least as strict, so it holds when render is wrong
or bypassed. Proven on a real API server: every rendered object admitted, 51 hostile objects refused (167 checks).

---

## Verdicts an engineer can act on

- **01 ACCEPT** in both envs.
- **02 PARTIAL:** the pod writes to `pp-settlement-events` through the shared role. The spec drops it, so the role is
  held. Fix: grant it, or `acknowledgeRemovals` with a reason.
- **03 REFUSE:** `default` project (and how to fix the ClusterRole), `s3:*`, `*`, `sts:AssumeRole`, another team's
  SSM, allow-all.
- **04 REFUSE:** prd without stg, a literal CIDR, `kms:CreateGrant` (use `kms:Decrypt`). Also: `kube-system`
  ingress won't admit an ALB (use `{albSubnets}`).

`specs/fixed/` follows each fix literally, and all eight come out clean ACCEPTs.

---

## How a change travels

- **Freight = (spec commit, chart version, image tag).** stg tests the exact combination prd will run.
- stg renders and syncs automatically. Events go to the gate.
- prd renders **from the Freight's spec commit** inside the promotion, after the gate passes. A human syncs.

Failure modes:
- **Passes, then rejected downstream:** a ClusterRole hits the AppProject; S3 sits behind a gateway endpoint → it
  shows up as `degraded`, and the gate holds the Freight.
- **Removes a used permission:** R8 against observed usage.
- **Same setting in two places:** one owner per field; the other place is refused.

---

## Migration: 35 services, no outage

1. Registry lines for all 35 (one MR; nothing changes in any cluster).
2. Specs drafted from the five files; teams review their own.
3. **Shadow render, diffed** against the hand-written objects, with the same names.
4. Cut over one service per commit: `Prune=false`, then adopt in place. Shared roles: new role, usage parity, switch
   the association.
5. Order: ingestion as canary → no-IAM services → single-team → cross-namespace together → shared roles last.
6. Delete the old files when: the diff is empty, the old app owns nothing, 7 days with no drift or AccessDenied, and
   30 days for shared roles.

---

## Soak gate: the soak gate

**Model:** fold events into *stints*, each an uninterrupted span of one revision being live and healthy.
"Promotable" = the stint containing `now` is at least 120 minutes long.

Ways the input misleads a naive gate, each with a test:
- duplicates, and a reused id (keep both);
- out of order, and a `+01:00` offset;
- events after `now`;
- prd events (r13 would look *exactly* 120 minutes old);
- `degraded` about a revision that is no longer live;
- adding up healthy time;
- rollbacks restart the clock;
- a truncated line → **fail closed, scoped:** "BLOCKED; otherwise WAIT until 12:45".

The tests caught seven deliberate naive mutations.

---

## What a trading venue changes

- **Brokers and venues are catalog entries (built).** `counterparty: venue-a-fix`: the platform holds the
  allow-listed addresses, ports and the teams cleared for it. Proven on a cluster against a stand-in gateway.
- **FIX sessions are stateful:** one active holder, rolled at the venue's session boundary.
- **Rollback never waits for the soak gate.** As written, the gate would block it.
- **Change windows:** no prd sync in market hours or settlement runs, except a rollback. One more gate input.
- **Audit for free:** spec commit → bot MR → gate verdict → who synced. The syncer is not the author.

---

## I attacked my own solution

- **Renderer:** 9 of 15 hostile specs accepted (another team's bucket, `msk-topic:main/*`, `s3:Put*`, a wildcard
  acknowledgement...). Fixed by **resource ownership** and exact rules.
- **Admission:** 8 of 11 hostile objects admitted, among them `s3:*` on `*` written as `"s3:\u002a"`. It was a weaker
  copy of the renderer. Now stricter than it, and on by default in every namespace.
- **Gate:** an `"STG"` event dropped silently, same-instant deploys resolved by id order. Now defects that fail
  closed.
- **Real cluster:** a CEL policy that kyverno accepted was invalid on the API server (`namespace` is reserved),
  and every prd Application had `syncPolicy: null`, which the Argo CD CRD refuses. Every offline check passed both.

Every attack is now a test.

---

## What I would build instead

Rules written in today's vocabulary (CIDRs, engineer-written IAM, minutes in stg) caused most of the holes.

- **Identity is the namespace and service account**, carried into AWS as Pod Identity session tags.
- **One static ABAC policy** instead of a role per service: nobody writes IAM, and R2 and R3 hold in AWS.
- **Owners grant access** in their own resource policies.
- **Infra in the chart** via a library chart, so Kargo promotes (chart, image) and infra soaks with code.
- **Admission policies are the one rule set**, also run in service CI.
- **Egress by host name**, proven on Cilium; **R8 at admission** by comparing old and new, proven on a real API
  server.

The cost is a CNI change and naming conventions. I'd propose it as an amendment to the rules.

---

## Scope, and what I would confirm first

**Next, after day one:**
- chart introspection;
- `svcrender import`;
- the usage collector (R8 reads a file);
- inventory checks (the registry refuses resources nobody owns);
- the exceptions UI.

**Confirm in a real account and with the team:**
- S3 gateway vs interface endpoints;
- MSK ACLs vs IAM auth;
- the ALB target type and real subnets;
- existing role names.

**Stand-ins for inputs the scenario leaves open:** catalog ids, load-balancer subnets, resource owners, observed
usage, topic names, pointers. All are labelled in `docs/notes.md`.
