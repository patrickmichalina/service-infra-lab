# Service Infrastructure Lab

A Go experiment in self-service Kubernetes infrastructure: validate service specs, render manifests,
and decide when a revision has soaked long enough in staging to promote to production.

| Component | Where |
|---|---|
| Design document (Architecture) | [`docs/design.md`](docs/design.md) |
| The tool, its tests, its output for all four requests in both envs (Renderer) | [`cmd/svcrender`](cmd/svcrender), [`internal/svc`](internal/svc), [`out/render/verdicts.txt`](out/render/verdicts.txt) |
| The promotion gate, its tests, its output for every query (Soak gate) | [`promotion`](promotion), [`cmd/soakgate`](cmd/soakgate), [`out/promotion/verdicts.txt`](out/promotion/verdicts.txt) |
| Working notes, including where the AI assistant was wrong | [`docs/notes.md`](docs/notes.md) |
| Admission, Argo CD project, network baseline and AWS backstops | [`policies/`](policies) |
| The same, run on real Kubernetes (kind, real ACK / External Secrets / Argo CD), with the transcript | [`tests/cluster/run.sh`](tests/cluster/run.sh), [`docs/evidence/cluster-run.txt`](docs/evidence/cluster-run.txt) |
| An adversarial pass over all of it, and what changed | [`docs/notes.md`](docs/notes.md#adversarial-pass-and-what-changed-because-of-it) |
| What running a trading venue changes: brokers and venues as catalog entries (built), sessions, rollback, change windows, audit | [`docs/design.md` §6](docs/design.md#6-what-a-trading-venue-changes) |
| What I would build instead, and the parts of it proven here | [`docs/design.md` §9](docs/design.md#9-what-i-would-build-instead), [`alternative/`](alternative) |

## Run it

You need Go 1.22 or later. Dependencies are vendored, so nothing needs the network or credentials, and nothing reads
the clock.

```sh
go test ./...                                     # every rule, every trap, golden outputs, determinism

go run ./cmd/soakgate                             # Soak gate over inputs/events (writes nothing)
go run ./cmd/svcrender all -specs specs -out out/render              # Renderer, the four sample requests
go run ./cmd/svcrender all -specs specs/fixed -out out/render-fixed  # ... after following the fixes

go run ./cmd/svcrender check -repo example-org/trader-ui -spec specs/trader-ui.yaml -env prd   # one spec, as CI would

policies/admission/test.sh                        # optional, needs the kyverno CLI; still offline
tests/cluster/run.sh                              # optional, needs Docker, kind, helm and the network: two real clusters, ~10 min
```

The committed `out/` is exactly what the code produces. CI ([`.github/workflows/ci.yml`](.github/workflows/ci.yml))
regenerates it and fails on any diff, and also runs the admission tests and the cluster suite.

## Service rendering verdicts

Each request was translated into a spec as the engineer asked, bad parts included ([`specs/`](specs)). Every refusal
names the rule, the place in the spec, and the exact text to write instead. Full text:
[`out/render/verdicts.txt`](out/render/verdicts.txt).

| Request | stg | prd | Why |
|---|---|---|---|
| 01 odds-ingest | ACCEPT | ACCEPT | Renders the role, network policy, secret and infra app. The chart Application follows Kargo's first promotion. Warning: its internet rule also carries S3 traffic, so name `aws:s3` in its hosts |
| 02 settlement-writer | PARTIAL | PARTIAL | **R8:** leaving the shared role would drop `kafka-cluster:WriteData` (and `DescribeTopic`) on `pp-settlement-events`, which the pod uses. The role is held until the spec grants it or acknowledges the removal; network and secrets render. Warnings: S3 is unreachable without 443 egress; nothing in `distribution` declares egress to 9090 |
| 03 rfq-router | REFUSE | REFUSE | **R1** `project: default`, plus how to fix the chart's ClusterRole. **R2** `s3:*`, `Resource: "*"` twice, `sts:AssumeRole`. **R3** reads a `state` parameter. **R4** allow-all ingress, allow-all egress that names no hosts. Warning: S3 is unreachable without 443 egress |
| 04 trader-ui | REFUSE | REFUSE | **R7** prd without stg. **R4/R5** literal `203.0.113.40/32` (use 443 internet egress naming the host). **R2** `kms:CreateGrant` is key management (use `kms:Decrypt`). Warnings: dropping stg deletes what runs there today (R8); `kube-system` ingress will not admit an ALB (use `{albSubnets}`); S3 is unreachable in prd |

[`specs/fixed/`](specs/fixed) applies those fixes literally. All four then come out ACCEPT in both envs with no
warnings ([`out/render-fixed/verdicts.txt`](out/render-fixed/verdicts.txt)), and a test keeps it that way.

## Promotion verdicts

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
[`docs/notes.md`](docs/notes.md#soak-gate-first-decision).

## Layout

```
docs/                design.md (Architecture), notes.md, slides.md;  docs/evidence/ the cluster-run transcripts
cmd/                 the two CLIs: svcrender (Renderer) and soakgate (Soak gate)
specs/               engineer-written specs, as requested;  specs/fixed/ after following the verdicts
platform/            platform-owned inputs: registry, catalog, observed usage (R8), Kargo pointers
inputs/              sample requests, events and environment configuration
internal/svc/        spec, rules (one function per rule), render, verdicts
promotion/           the soak gate library;  cmd/soakgate is its CLI
policies/            admission (VAP + offline tests), Argo CD projects, namespace default-deny, AWS boundary and controller policy
tests/cluster/       the real-cluster suite (kind, plus a Cilium cluster) and its test charts
alternative/         the alternative design's admission, network and IAM pieces
out/                 committed outputs
.github/workflows/   CI: tests, output drift, admission tests, cluster suite
```

## Validation and limitations

This lab uses illustrative infrastructure data and has no connected AWS account or production data.
The checks below distinguish cluster validation from assumptions that still need a real AWS environment.

- **Kubernetes: on real clusters.** `tests/cluster/run.sh` makes 167 checks on throwaway kind clusters, which it
  deletes afterwards ([transcript](docs/evidence/cluster-run.txt)):
  - every rendered object is applied to a real API server (Kubernetes 1.37), with the real ACK, External Secrets and
    Argo CD CRDs and strict field validation;
  - all 51 hostile objects are refused, each by the policy meant to refuse it;
  - the ExternalSecrets sync;
  - Argo CD refuses the platform's kinds in team charts;
  - every network rule is checked on a policy-enforcing CNI;
  - a counterparty rule reaches a stand-in venue gateway only on its port, and only from the cleared team;
  - the alternative design's FQDN egress is checked on Cilium.

  The run also caught two bugs that no offline check could see
  ([notes](docs/notes.md#where-the-ai-assistant-was-wrong-and-what-caught-it)).
- **AWS: from primary sources.** IAM, Pod Identity, S3 endpoint and MSK behaviour are taken from AWS documentation
  and the VPC CNI's source, each cited
  ([notes](docs/notes.md#things-verified-by-documentation-not-by-running-them)). They are the first things to
  confirm in a real account ([design §8](docs/design.md#8-unknowns-i-would-verify-first)).
- **Inputs the scenario leaves open** are filled in with labelled stand-ins: catalog ids, load-balancer subnets,
  resource owners, observed usage, topic names and pointer versions
  ([notes](docs/notes.md#illustrative-data-and-assumptions)).
- **Scope for day one:** chart introspection, `svcrender import`, inventory checks and the usage collector come
  next ([design §7](docs/design.md#7-what-i-cut-from-day-one-and-why)).
