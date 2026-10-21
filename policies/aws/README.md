# Cloud-side backstops (R2)

Not applied or tested here; they show where AWS itself holds R2 when everything above it
(spec review, renderer, admission) has been bypassed.

- `service-boundary.json` is the permissions boundary every service role carries. It denies
  IAM, STS, Organizations and KMS key management outright, whatever an inline policy says.
- `ack-iam-controller.json` is the policy of the ACK IAM controller, the only principal that
  creates service roles. AWS refuses to create a `svc-*` role without the boundary, refuses
  to remove the boundary, and refuses to edit the boundary policy.

Not covered here: `Resource: "*"` and service-wide wildcards cannot be refused by IAM itself
at policy-write time; they are refused by the renderer (R2) and again at admission
(`policies/admission/iam-role.yaml`). The boundary limits their blast radius.
