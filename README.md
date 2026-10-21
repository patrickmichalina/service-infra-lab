# Platform take-home: self-service service infrastructure

| Deliverable | Where |
|---|---|
| Design document (Part A) | [`docs/design.md`](docs/design.md) |
| The tool, its tests, its output for all four requests in both envs (Part B) | [`cmd/svcrender`](cmd/svcrender), [`internal/svc`](internal/svc), [`out/render/verdicts.txt`](out/render/verdicts.txt) |
| The promotion gate, its tests, its output for every query (Part C) | [`promotion`](promotion), [`cmd/soakgate`](cmd/soakgate), [`out/promotion/verdicts.txt`](out/promotion/verdicts.txt) |
| Working notes, including where the AI assistant was wrong | [`docs/notes.md`](docs/notes.md) |
| Admission, Argo CD project and AWS backstops | [`policies/`](policies) |

## Run it

You need Go 1.22 or later. Dependencies are vendored, so nothing needs the network or credentials, and nothing reads
the clock.

```sh
go test ./...                                     # every rule, every trap, golden outputs, determinism

go run ./cmd/soakgate                             # Part C over inputs/events (writes nothing)
go run ./cmd/svcrender all -specs specs -out out/render              # Part B, the four requests as asked
go run ./cmd/svcrender all -specs specs/fixed -out out/render-fixed  # ... after following the fixes

go run ./cmd/svcrender check -repo example-org/trader-ui -spec specs/trader-ui.yaml -env prd   # one spec, as CI would

policies/admission/test.sh                        # optional, needs the kyverno CLI; still offline
```

The committed `out/` is exactly what the code produces. CI regenerates it and fails on any diff.

## Part B verdicts

Each request was translated into a spec as the engineer asked, bad parts included ([`specs/`](specs)). Every refusal
names the rule, the place in the spec, and the exact text to write instead. Full text:
[`out/render/verdicts.txt`](out/render/verdicts.txt).

| Request | stg | prd | Why |
|---|---|---|---|
| 01 odds-ingest | ACCEPT | ACCEPT | Renders the role, network policy, secret and infra app. The chart Application follows Kargo's first promotion |
| 02 settlement-writer | PARTIAL | PARTIAL | **R8:** leaving the shared role would drop `kafka-cluster:WriteData` (and `DescribeTopic`) on `pp-settlement-events`, which the pod uses. The role is held until the spec grants it or acknowledges the removal; network and secrets render. Warnings: S3 is unreachable without 443 egress; nothing in `distribution` declares egress to 9090 |
| 03 rfq-router | REFUSE | REFUSE | **R1** `project: default`, plus how to fix the chart's ClusterRole. **R2** `s3:*`, `Resource: "*"` twice, `sts:AssumeRole`. **R3** reads a `state` parameter. **R4** allow-all ingress, allow-all egress |
| 04 trader-ui | REFUSE | REFUSE | **R7** prd without stg. **R4/R5** literal `203.0.113.40/32` (use 443 internet egress with a reason). **R2** `kms:CreateGrant` is key management (use `kms:Decrypt`). Warning: `kube-system` ingress will not admit an ALB |

[`specs/fixed/`](specs/fixed) applies those fixes literally. All four then come out ACCEPT in both envs with no
warnings ([`out/render-fixed/verdicts.txt`](out/render-fixed/verdicts.txt)), and a test keeps it that way.

## Part C verdicts

Soak is 120 minutes in stg (`inputs/platform-config/prd.yaml`). Full reasons:
[`out/promotion/verdicts.txt`](out/promotion/verdicts.txt).

| now | service | rev | verdict | in short |
|---|---|---|---|---|
| 12:00 | odds-ingest | r14 | PROMOTABLE | live since 08:00 |
| 12:00 | odds-ingest | r13 | BLOCKED | replaced by r14 at 08:00. Counting prd's 10:00 redeploy would make it look exactly 120 minutes old |
| 13:00 | odds-ingest | r14 | BLOCKED | replaced by r15 at 12:30; the soak must end at `now` |
| 13:00 | odds-ingest | r15 | WAIT | promotable at 14:30 |
| 10:40 | settlement-writer | r7 | BLOCKED | degraded at 10:30. The 10:45 recovery is in the future |
| 12:00 | settlement-writer | r7 | BLOCKED | an unreadable event (e-050, stg, 11:05, `settlem…`) may concern it. Otherwise WAIT until 12:45: recovered at 10:45, and healthy time does not add up |
| 12:00 | rfq-router | r3 | WAIT | rolled back at 10:30, so the clock restarted. Promotable at 12:30 |
| 12:00 | rfq-router | r4 | BLOCKED | replaced by r3 at 10:30 |
| 12:00 | trader-ui | r22 | BLOCKED | never in stg; already in prd without soaking |
| 12:00 | book-builder | r9 | PROMOTABLE | deployed 10:30+01:00 = 09:30Z. r8's 10:00 `degraded` is about r8 |
| 12:00 | book-builder | r10 | BLOCKED | never deployed to stg |

Every input defect is reported in the output, and none is dropped silently. The handling of each is argued in
[`docs/notes.md`](docs/notes.md#part-c-first-decision).

## Layout

```
specs/               engineer-written specs, as requested;  specs/fixed/ after following the verdicts
platform/            platform-owned inputs: registry, catalog, observed usage (R8), Kargo pointers
inputs/              the brief's inputs, unchanged
internal/svc/        spec, rules (one function per rule), render, verdicts
promotion/           the soak gate library;  cmd/soakgate is its CLI
policies/            admission (VAP + offline tests), Argo CD projects, AWS boundary and controller policy
out/                 committed outputs
```

## Honesty

- **Not run against a real cluster or AWS.** The ACK field names, the Pod Identity trust conditions and the agent
  egress baseline are unverified. [`docs/design.md` §8](docs/design.md#8-unknowns-i-would-verify-first) lists what I
  would check first.
- **Some inputs are invented** (catalog ids, observed usage, topic names, pointer versions). Each is listed in
  [`docs/notes.md`](docs/notes.md#things-i-invented-the-brief-gives-none-of-them).
- **Not built:** `svcrender import`, chart introspection, the usage collector, inventory existence checks
  ([design §7](docs/design.md#7-what-i-cut-from-day-one-and-why)).
