# Request: trader-ui (distribution team)

Existing service. Repo `example-org/trader-ui`, chart `trader-ui`, image `trading/trader-ui`.
Runs in `distribution`.

Change: we only need this in production now; please remove the staging deployment, it costs
money and nobody uses it. Also add egress to `203.0.113.40/32` on 443 for the new auth provider,
and open ingress on 3000 from the internal ALB in `kube-system`.

Secrets: `/<env>/distribution/trader-ui/session-key`. IAM: it needs `s3:GetObject` on
`trading-platform-<env>-ui-assets/*` and `kms:CreateGrant` on the assets key.
