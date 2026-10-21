# Current state, and the rules the platform must keep

## Environments

| env | AWS account | cluster | VPC CIDR | SSM prefix | Argo CD |
|---|---|---|---|---|---|
| stg | 222222222222 | trading-platform-stg | 10.20.0.0/16 | `/stg/` | auto-sync on |
| prd | 333333333333 | trading-platform-prd | 10.30.0.0/16 | `/prd/` | auto-sync **off**, a human syncs |

Images are built once by CI and pushed to the dev account's ECR (`111111111111`). stg's ECR is
a pull-through cache of dev, prd's is a pull-through cache of stg. Kargo promotes image tags and
chart versions by committing pointer files into `platform-gitops`:
`services/image-tags/<env>/<svc>.yaml` and `apps/chart-versions/<env>/<svc>.yaml`. Each service
owns a Helm chart in its own repository, published as an OCI chart to dev ECR.

## Teams and namespaces

| team | namespace | Argo CD project | what runs there |
|---|---|---|---|
| ingestion | `ingestion` | `ingestion` | market-data gateways, odds ingesters |
| state | `state` | `state` | order management, settlement, reconciliation |
| normalization | `normalization` | `normalization` | book builders, enrichers |
| distribution | `distribution` | `distribution` | trader-facing UI and APIs |

A team may only deploy into its own namespace. The Argo CD project of the same name restricts
destinations to that namespace and the resource kinds a service needs. The `default` project is
unrestricted and exists only for platform components.

## What the five files do today

1. **Argo CD `Application`**: points at the service's OCI chart at the promoted version, with
   the promoted image tag as a Helm value, into the team's namespace, under the team's project.
2. **ECR repository** in dev, with pull-through rules in stg and prd.
3. **IAM role** per service, trusted by the pod's ServiceAccount through EKS Pod Identity, with
   the platform **permissions boundary** attached (`arn:aws:iam::<acct>:policy/service-boundary`).
   Policies are hand-written by the platform team from a description the service team gives.
4. **`NetworkPolicy`**: default is deny-all ingress and egress in every namespace; each service
   declares what it needs. Policies are additive across objects, so a broad one anywhere widens
   the namespace.
5. **`ExternalSecret`** mapping SSM parameters into a Kubernetes Secret in the namespace.

## Rules the platform team must keep holding

These are the invariants. The new design has to hold each one somewhere, and say where.

- **R1** A service deploys only into its team's namespace and only under its team's Argo CD
  project. Nothing a service repository contains may reach the `default` project or another
  namespace.
- **R2** A service's IAM role carries the permissions boundary, and its inline policy may not
  use `Resource: "*"` or an action wildcard broader than a single service prefix (`s3:Get*` is
  fine; `s3:*` and `*` are not). IAM, STS and KMS key-management actions are never grantable
  from a spec.
- **R3** A service reads only SSM parameters under `/<env>/<team>/`. A spec names the path
  **without** the env; the platform injects it.
- **R4** A `NetworkPolicy` from a spec selects only that service's pods, never an empty
  `podSelector`. Ingress from another namespace must name that namespace. Egress to the VPC must
  use the `{vpcCidr}` token, never a literal CIDR. Egress to `0.0.0.0/0` is allowed only on
  443 and only when the spec says why.
- **R5** Account IDs, CIDRs, cluster names and role ARNs never appear in a service repository.
- **R6** A spec change reaches prd only after it has been live in stg.
- **R7** A service declares every environment it runs in explicitly. A service in prd must also
  be in stg.
- **R8** A permission the running pod uses must not be removable without the engineer seeing
  what they are removing.

Some of these cannot be fully decided at render time. Part of the exercise is saying which.
