# Request: settlement-writer (state team)

Existing service, already deployed by hand. Repo `example-org/settlement-writer`, chart and
image of the same name. Runs in `state`, both envs.

Change: we are moving from the shared `data-services` IAM role to our own. We need
`rds-db:connect` to the `settlements` RDS instance (user `settlement_writer`), read and write on
the `trading-platform-<env>-settlement-reports` bucket, and Kafka as before (MSK, IAM, 9098).
Secrets: `/<env>/state/settlement-writer/db-password` and
`/<env>/state/settlement-writer/report-signing-key`.

Network: ingress on 8080 from `monitoring`, plus 9090 from the `distribution` namespace, because
`trader-ui` calls our report endpoint. Egress: RDS (5432, VPC), Kafka (9098, VPC), and that's it.

(Platform team note added later: this pod also publishes to the `pp-settlement-events` topic
using the shared role's MSK ACLs; the request above does not mention Kafka write.)
