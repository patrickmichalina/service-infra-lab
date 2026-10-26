# Request: rfq-router (normalization team)

New service. Repo `example-org/rfq-router`, chart `rfq-router`, image `trading/rfq-router`.
Runs in `normalization`, both envs.

It's a small router, it just needs to talk to everything: `s3:*` on all buckets (we don't know
yet which ones we'll use), Kafka, and `sts:AssumeRole` so it can reach the data account later.
Please put it in the `default` Argo project, the `normalization` one keeps rejecting our
chart's ClusterRole.

Network: allow all ingress, we have a few callers and don't want to keep editing this. Egress:
allow all.

Secrets: `/<env>/state/rfq-router/venue-credentials` (the state team set it up for us).
