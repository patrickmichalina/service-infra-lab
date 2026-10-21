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
- **Buckets and callers in the fixed rfq-router:** `rfq-audit`, and callers in its own namespace.
- **Assumptions, unverified:**
  - S3 is reached through a **gateway** endpoint (drives the S3 warning).
  - The Pod Identity agent's link-local address `169.254.170.23:80` needs an egress baseline.
  - Trust-policy conditions on Pod Identity session tags (`aws:RequestTag/kubernetes-namespace`, …) work as rendered.
  - ACK resource fields (`Role.spec.inlinePolicies`, `PodIdentityAssociation.spec.roleARN`).
  - The `ack-services` and `services-infra` names.

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
9. **Smaller things the tests caught:**
   - a test expected WAIT where 180 minutes was already PROMOTABLE (the test was wrong);
   - fixture ordering put appended permissions inside `secrets`;
   - golden files embedded a temp path;
   - a shell loop relied on word-splitting that zsh does not do, which produced garbled CRD stubs.

## Open questions I would ask the team

- Do MSK clients use IAM auth only, or IAM plus ACLs? That decides whether R8 for Kafka needs MSK logs.
- How are the existing role names formed? Migration needs them for adoption.
- Is there an internal ALB subnet range? That would let the platform offer a `{albSubnets}` token instead of
  `{vpcCidr}`.
- Who owns `/<env>/state/rfq-router/venue-credentials` today, and why is it under `state`?
