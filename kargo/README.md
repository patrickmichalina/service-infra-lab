# Native Kargo promotion branch

This branch uses Kargo 1.8.3 Stage, Freight, Promotion, requiredSoakTime and
Argo Rollouts AnalysisTemplate resources. The original `promotion/` library and
`soakgate` CLI remain as the assignment's historical event-replay deliverable;
they are not called by this pipeline.

Render: `helm template platform kargo`. Test on disposable Kubernetes:
`tests/kargo/run.sh` (Docker, kind, kubectl, Helm, Python 3, network required).
The test installs real Kargo and Argo Rollouts controllers. It substitutes an
HTTP endpoint for deployment side effects and successful/failing Jobs for the
monitoring provider. It does not manufacture controller status. Production Git
pushes, Argo CD syncing and Prometheus integration are not covered by that test.

## Delivery contract

Set bundleRepo and prometheus in values.yaml. A trusted build publishes a Git
commit containing validated `bundles/<env>/<service>/` manifests for both
environments, derived from the same spec/chart/image combination. Run svcrender
and admission checks before publishing; preserve the exact image/chart versions
in those bundles. The bundled infrastructure and workload references travel as
one immutable Git commit (Kargo Freight). This publishing integration requires
your actual repository and credentials; the example URL is a placeholder.

Each service has its own Warehouse and staging/production Stages. Promotion uses
Kargo's git-clone, git-clear, copy, git-commit and git-push steps to update a
service/environment branch. Configure a platform-owned Argo Application for each
branch. Staging is named `<service>-stg-bundle`, auto-syncs, and must authorize
`platform-promotion:<service>-stg` in its
`kargo.akuity.io/authorized-stage` annotation. It must expose health of the
workload and infrastructure, including custom-resource health checks where needed.
Production tracks `<service>-prd` and retains manual sync; its Stage intentionally
contains no argocd-update step. Stage promotion publishes the desired revision;
a human still syncs production.

Staging verification samples Argo's Healthy/Synced metric 120 times, once per
minute. Missing/unhealthy results or provider errors fail verification. Recovery
requires Kargo re-verification, which creates a fresh AnalysisRun. A production
Stage requires successful upstream verification and a native 120m soak. No
auto-promotion policy is installed. Limit manual Freight approval permissions:
Kargo explicitly allows approval to bypass verification and soak.

## Semantic differences from the assignment

This implements Kargo's release-qualification policy, not historical JSONL replay.
Kargo 1.8.3 `Freight.HasSoakedIn` uses `GetLongestSoak`: a completed verified soak
can qualify an older Freight after it has left staging. It need not be the
currently live revision at the instant production is requested. Native soak
tracks occupancy, not continuous health. Verification is sampled and successful
verification remains recorded; it is not an uninterrupted-health proof ending at
an arbitrary supplied `now`. An outage after successful verification does not
revoke that qualification automatically. These are explicit policy changes;
retain the original gate if the assignment's exact semantics are mandatory.

Sources:
- https://docs.kargo.io/user-guide/how-to-guides/verification
- https://github.com/akuity/kargo/blob/v1.8.3/api/v1alpha1/freight_types.go
- https://docs.kargo.io/operator-guide/basic-installation

## Applying to an existing Kargo installation

Install the pinned Kargo and Argo Rollouts controllers first. Render with a values
file containing your real repository and monitoring endpoints:

```sh
helm template platform kargo -f deployment-values.yaml > pipeline.yaml
kubectl apply -f - <<'YAML'
apiVersion: kargo.akuity.io/v1alpha1
kind: Project
metadata:
  name: platform-promotion
YAML
kubectl wait --for=create namespace/platform-promotion --timeout=60s
kubectl apply -f pipeline.yaml
```

Supply repository credentials through Kargo's normal project-scoped credential
Secrets. Restrict bundle publication to trusted build jobs. Promote with Kargo's
UI or CLI; use its reverify operation after fixing a failed staging verification.
Keep the original brief unchanged: this branch deliberately explores an
operational alternative rather than claiming to meet Part C's replay contract.

## Recorded validation

The reproducible cluster transcript is in `docs/evidence/kargo-run.txt`.
Tests cover rejection before staging, rejection before soak completion,
successful promotion after soak, failed verification preventing promotion, and
Kargo's eligibility of a previously qualified revision after staging changes.
All four service configurations are also submitted to server-side dry-run.
