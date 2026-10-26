# The alternative design, as far as it can be proven here

[`docs/design.md` §9](../docs/design.md#9-what-i-would-build-instead) argues for a simpler design that puts every
rule at a chokepoint no team can route around. These files are its pieces that can exist without an AWS account,
and what has been proven about each.

| File | What it is | Proven how |
|---|---|---|
| [`admission/acknowledge-removal.yaml`](admission/acknowledge-removal.yaml) | R8 without usage data or a renderer: an update that drops a network rule or an IAM action is refused unless an annotation says why | On a real API server ([`tests/cluster/run.sh`](../tests/cluster/run.sh)): dropping an egress rule and dropping `kafka-cluster:WriteData` are refused; the same updates with the annotation are admitted |
| [`network/fqdn-egress.yaml`](network/fqdn-egress.yaml) | Internet egress by host name instead of `0.0.0.0/0:443`: what a spec's `hosts:` becomes | On a Cilium cluster in the same run: the named host is reachable on 443, another host and a bare address are not |
| [`iam/service-abac-policy.stg.json`](iam/service-abac-policy.stg.json) | One static identity policy for every service role, keyed on EKS Pod Identity session tags: own team's parameters, buckets, topics and keys; own database user and consumer groups | **Not executed** (no AWS account). Each mechanism is documented: session tags as `${aws:PrincipalTag/kubernetes-namespace}` ([EKS](https://docs.aws.amazon.com/eks/latest/userguide/pod-id-abac.html)); variables in the resource part of an ARN ([IAM](https://docs.aws.amazon.com/IAM/latest/UserGuide/reference_policies_variables.html)); the `rds-db` ARN format ([RDS](https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/UsingWithRDS.IAMDBAuth.IAMPolicy.html)) |
| [`iam/owner-share-bucket-policy.stg.json`](iam/owner-share-bucket-policy.stg.json) | Cross-team access granted by the owner, in the owner's bucket policy, for one consumer's session tags | **Not executed.** Same-account access is allowed by either the identity or the resource policy, so this works only because the identity policy above grants no bucket outside the team's own prefix |

What the ABAC policy assumes, and would be checked first in an AWS account (`aws iam simulate-custom-policy` with
session tags as context keys, then a real pod): buckets, topics and consumer groups are named with the team or
service as a prefix; KMS keys carry a `team` tag; database users are named after the service account.
