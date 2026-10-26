# Request: odds-ingest (ingestion team)

New service. Repo `example-org/odds-ingest`, chart `odds-ingest`, image `trading/odds-ingest`.
Runs in `ingestion`, both envs.

It consumes a vendor's HTTPS odds feed (`feed.vendor-example.com`, port 443), writes to our
Kafka cluster (MSK, IAM auth, broker port 9098, inside the VPC), and keeps a daily snapshot in an
S3 bucket the data team already created: `trading-platform-<env>-odds-snapshots`. Needs to
write objects there and list the bucket. Needs the vendor API key, which is already in SSM at
`/<env>/ingestion/odds-ingest/vendor-api-key`.

Prometheus scrapes it on 8080 from the `monitoring` namespace. Nothing else talks to it.
