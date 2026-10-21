---
marp: true
title: Self-service service infrastructure
paginate: true
---

# Taking the platform team out of the path

Routine change: four days and two people today. Minutes and zero people, with R1–R8 still holding.

- Part A: the design (`docs/design.md`)
- Part B: `svcrender`, the rules and the renderer
- Part C: the soak gate

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
  - {cidr: 0.0.0.0/0, ports: [443], reason: vendor feed}
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
| R1 namespace/project | registry keyed by *repo* | AppProject + VAP | – |
| R2 IAM | action/resource checks | VAP: boundary, no `*` | boundary; controller can't create without it |
| R3 SSM | path vs team, env injected | store scoped to team | store role |
| R4 network | rendered from intent | VAP: shape | – |
| R5 env facts | scan every value | – | – |
| R6 stg first | prd only from the promoted spec | – | – (the **gate** decides) |
| R7 envs | refused | – | – |
| R8 used permissions | grants vs **observed usage** | – | usage data source |

Render can say *what to write instead*. The layers after it hold even when render is wrong or bypassed.

---

## Verdicts an engineer can act on

- **01 ACCEPT** in both envs.
- **02 PARTIAL:** the pod writes to `pp-settlement-events` through the shared role. The spec drops it, so the role is
  held. Fix: grant it, or `acknowledgeRemovals` with a reason.
- **03 REFUSE:** `default` project (and how to fix the ClusterRole), `s3:*`, `*`, `sts:AssumeRole`, another team's
  SSM, allow-all.
- **04 REFUSE:** prd without stg, a literal CIDR, `kms:CreateGrant` (use `kms:Decrypt`). Also: `kube-system`
  ingress won't admit an ALB.

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

## Part C: the soak gate

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

## What I cut, and what I don't know

**Cut:**
- chart introspection;
- `svcrender import`;
- the usage collector (R8 reads a file);
- inventory checks;
- resource ownership (any team can name any bucket);
- the exceptions UI.

**Would verify first:**
- the CNI and the Pod Identity agent egress;
- S3 gateway vs interface endpoints;
- MSK ACLs vs IAM auth;
- the ALB target type;
- session-tag trust conditions;
- existing role names.

**Invented inputs:** catalog ids, observed usage, topic names, pointers. All are listed in `docs/notes.md`.
