# May

> **Mây** means "cloud" in Vietnamese.

May is a local cloud emulator compatible with the AWS APIs. It is a single
dependency-free Go binary that listens on port 4566, so any AWS client (AWS CLI, boto3,
the Go SDK, Terraform, CDK, Pulumi) can point at it with
`endpoint_url=http://localhost:4566`.

- No account, no auth token, no telemetry. MIT licensed.
- Starts in tens of milliseconds and idles under 10 MB of memory.
- Every supported operation is tested with real SDKs; gaps are listed below.

## Quick start

```bash
go build -o may ./cmd/may && ./may          # or: docker build -t maycloud/may . && docker run -p 4566:4566 maycloud/may

export AWS_ACCESS_KEY_ID=test AWS_SECRET_ACCESS_KEY=test AWS_DEFAULT_REGION=us-east-1
aws --endpoint-url http://localhost:4566 sts get-caller-identity
aws --endpoint-url http://localhost:4566 iam create-role --role-name app \
    --assume-role-policy-document file://trust.json
aws --endpoint-url http://localhost:4566 s3 mb s3://demo
aws --endpoint-url http://localhost:4566 s3 cp ./big-file.zip s3://demo/
aws --endpoint-url http://localhost:4566 sqs create-queue --queue-name jobs
aws --endpoint-url http://localhost:4566 dynamodb create-table --table-name users \
    --attribute-definitions AttributeName=id,AttributeType=S --key-schema AttributeName=id,KeyType=HASH \
    --billing-mode PAY_PER_REQUEST
```

Any credentials are accepted. The account ID is always `000000000000`.

### Options

| Flag | Env | Default | |
|---|---|---|---|
| `-port` | `MAY_PORT` | `4566` | Port to listen on |
| `-host` | `MAY_HOST` | `0.0.0.0` | Address to bind |
| `-region` | `MAY_REGION` | `us-east-1` | Region for resources created by unsigned requests |
| `-hostname` | `MAY_HOSTNAME` | | Extra hostnames for virtual-host S3 (`bucket.<hostname>`) |
| `-quiet` | `MAY_QUIET` | off | Don't log each request |
| `-docker` | `MAY_DOCKER` | off | Real mode: run EC2 instances and RDS databases as Docker containers |

### Utility endpoints

- `GET /_may/health`: running services and version
- `POST /_may/reset`: wipe all state (in real mode, also removes instance containers)
- `GET /_may/ec2/instances`: in real mode, how to reach each instance (SSH command, published ports)
- `GET /_may/rds/instances`: in real mode, how to connect to each database (port, `psql`/`mysql` command)

Both are also served under `/_localstack/` for existing tooling.

### Terraform

```hcl
provider "aws" {
  region                      = "us-east-1"
  access_key                  = "test"
  secret_key                  = "test"
  skip_credentials_validation = true
  skip_requesting_account_id  = true
  s3_use_path_style           = true
  endpoints {
    dynamodb = "http://localhost:4566"
    ec2      = "http://localhost:4566"
    iam      = "http://localhost:4566"
    logs     = "http://localhost:4566"
    rds      = "http://localhost:4566"
    s3       = "http://localhost:4566"
    sqs      = "http://localhost:4566"
    sts      = "http://localhost:4566"
  }
}
```

See [`test/terraform/`](test/terraform/) for full examples.

## Supported services

### STS
GetCallerIdentity, AssumeRole, GetSessionToken. Temporary keys are
remembered, so GetCallerIdentity returns the assumed-role or IAM-user ARN
for the key that signed the request.

### IAM
- Roles: Create, Get, List, Delete, Update, UpdateRoleDescription, UpdateAssumeRolePolicy, tags
- Users: Create, Get, List, Update, Delete, tags; access keys (Create, List, Update, Delete, GetLastUsed)
- Groups: Create, Get, List, Delete, AddUserToGroup, RemoveUserFromGroup, ListGroupsForUser
- Managed policies: Create, Get, List, Delete, versions (Create, Get, List, Delete, SetDefault), tags, ListEntitiesForPolicy
- Attached and inline policies on roles, users and groups
- Instance profiles: Create, Get, List, Delete, Add/RemoveRole, ListInstanceProfilesForRole, tags
- Built-in AWS managed policies (`AdministratorAccess`, `ReadOnlyAccess`, `AmazonS3FullAccess`,
  `service-role/AWSLambdaBasicExecutionRole` and about 25 more)

Policy documents are validated as JSON and returned URL-encoded, like AWS.
`DeleteConflict`, `EntityAlreadyExists`, `NoSuchEntity`, `LimitExceeded` and
`MalformedPolicyDocument` behave as in AWS, and lists paginate with `Marker`.

### S3
- Path-style and virtual-host-style (`bucket.localhost`, `bucket.s3.localhost.localstack.cloud`) addressing
- Buckets: Create, List, Head, Delete, GetBucketLocation
- Objects: Put, Get, Head, Delete, DeleteObjects, Copy, tagging; range and `partNumber` GETs;
  conditional requests (`If-Match`, `If-None-Match`, dates); metadata and `response-*` overrides
- Listing: ListObjectsV2, ListObjects, ListObjectVersions, with prefix, delimiter, pagination and `encoding-type=url`
- Multipart: Create, UploadPart, UploadPartCopy, Complete, Abort, ListParts, ListMultipartUploads
- `aws-chunked` uploads with trailing checksums; Content-MD5 and `x-amz-checksum-*` are verified
- Bucket configuration: tagging, policy, versioning status, CORS and encryption are stored and returned.
  Lifecycle, website, logging, replication, object lock, accelerate, request payment, ACL,
  ownership controls, public access block and notifications return AWS's defaults or
  "not configured" errors, and store whatever is PUT.

### SQS
Both protocols: JSON (current SDKs) and query/XML (older SDKs, AWS CLI before 2.14), with
the legacy error codes that clients match on (`AWS.SimpleQueueService.NonExistentQueue`).
- Queues: Create, Delete, GetQueueUrl, ListQueues (paginated), Get/SetQueueAttributes, Purge, tags,
  Add/RemovePermission, ListDeadLetterSourceQueues
- Messages: Send, Receive, Delete, ChangeMessageVisibility, and their batch versions
- Visibility timeouts, delay seconds, long polling (`WaitTimeSeconds`), retention,
  message and system attributes with the MD5 checksums SDKs verify
- FIFO queues: message groups delivered in order (a group is blocked while a message is in flight),
  deduplication by ID or content, `messageGroup` deduplication scope, sequence numbers
- Dead-letter queues (`RedrivePolicy`) and redrive back with StartMessageMoveTask

Queue URLs use the host the client called: `http://localhost:4566/000000000000/<name>`.

### DynamoDB
- Tables: Create, Describe, List, Update (billing mode, throughput, GSI create/update/delete,
  streams, SSE, table class, deletion protection), Delete; tags; TTL settings;
  point-in-time recovery settings; DescribeLimits, DescribeEndpoints
- Global and local secondary indexes with `ALL`, `KEYS_ONLY` and `INCLUDE` projections
- Items: Put, Get, Update, Delete with conditions, all `ReturnValues` options and
  `ReturnValuesOnConditionCheckFailure`; 400 KB item limit; numbers with 38-digit precision
- Query and Scan: key conditions, filters, projections, `Select`, `Limit`, 1 MB pages,
  `ExclusiveStartKey`, reverse order, parallel scan segments
- BatchGetItem, BatchWriteItem, TransactGetItems, TransactWriteItems (with
  `CancellationReasons` and idempotency tokens)
- The full expression language: conditions and functions, projections with nested paths,
  `SET`/`REMOVE`/`ADD`/`DELETE` updates with `if_not_exists`, `list_append` and arithmetic,
  placeholder validation (unused or undefined names and values are rejected, as in AWS)
- Legacy parameters: `Expected`, `AttributeUpdates`, `KeyConditions`, `QueryFilter`,
  `ScanFilter`, `AttributesToGet`, `ConditionalOperator`
- `ReturnConsumedCapacity` returns estimated units

### CloudWatch Logs
- Log groups (retention, KMS key, class, deletion protection, tags by name or ARN) and log streams
- PutLogEvents with AWS's validation (order, batch size and span, too-old/too-new/expired events);
  GetLogEvents with forward/backward tokens; FilterLogEvents across streams with paging
- The filter pattern language: terms, `"phrases"`, `-exclusions`, `?alternatives`, `%regex%`,
  JSON patterns (`{ $.level = "error" && $.ms > 100 }`) and space-delimited patterns
  (`[ip, user, ..., status = 5*]`). `aws logs tail` works.
- Metric and subscription filters are stored (no metrics or deliveries are produced)

### EC2 (API only by default)
- Each region starts with a default VPC (three default subnets, internet gateway, main route table,
  default security group and network ACL), like a new AWS account
- VPCs and attributes, subnets (CIDR validation and IP allocation), internet gateways, route tables, routes
  and associations, default network ACLs, DHCP options, availability zones and regions
- Security groups with rules (IpPermissions and per-rule `sgr-` IDs, DescribeSecurityGroupRules,
  ModifySecurityGroupRules); key pairs (RSA/ED25519 creation and import, with AWS's fingerprints)
- A fixed AMI catalog (Amazon Linux 2023 and 2, Ubuntu 24.04 and 22.04, Debian 12; x86_64 and arm64) with
  real naming patterns, so `data "aws_ami"` lookups work; common instance types (t2/t3/t3a/t4g, m/c/r families)
- RunInstances (subnets, security groups, key pairs, user data, block devices, IAM instance profiles,
  metadata options, client tokens), DescribeInstances with filters and pagination, start/stop/reboot/terminate
  with realistic state changes, instance attributes, credit specifications, console output, volumes and
  network interfaces; tags and filters on everything

### EC2 real mode (`-docker`)
With `may -docker`, each instance is a container you can use:
- The AMI picks the image (`ubuntu:24.04`, `amazonlinux:2023`, `debian:12`, ...); the instance type limits CPU and memory
- Each VPC becomes a Docker network with the VPC's CIDR, and the container gets the instance's private IP,
  so instances in a VPC reach each other
- User data (`#!` scripts) runs on first boot; with a key pair, sshd is installed for the image's default
  user (`ubuntu`, `ec2-user`, `admin`) with passwordless sudo
- TCP ports opened by the instance's security groups (and 22 with a key pair) are published on 127.0.0.1;
  `GET /_may/ec2/instances` lists them, or use `docker exec -it may-<instance-id> bash`
- Inside the instance, `AWS_ENDPOINT_URL` points at may and the metadata service (IMDSv1/v2) serves
  instance data, user data, the identity document and instance-profile credentials, which STS recognizes
  as the assumed role
- Stop keeps the container's disk; terminate removes it; a container that exits on its own is an
  instance-initiated shutdown. Containers are removed when may exits.

### RDS (API only by default)
- PostgreSQL (13-17), MySQL (8.0, 8.4) and MariaDB (10.6, 10.11, 11.4); DescribeDBEngineVersions.
  `EngineVersion` may be a major line (`16`) or a full version
- DB instances: Create, Describe (filters, pagination), Modify (class, storage, password, port, parameter
  group, security groups, subnet group, backups, rename and more), Delete (final snapshot, deletion
  protection), Start, Stop, Reboot, with realistic statuses (`creating`, `modifying`, `stopping`, ...)
- Instances are placed in a VPC: DB subnet groups (Create, Describe, Modify, Delete; subnets must exist in
  EC2 and cover two AZs) or the default VPC's `default` group, and VPC security groups (the VPC's default
  group if none is given)
- DB parameter groups: Create, Describe, Modify, Reset, Delete, DescribeDBParameters, and the
  `default.<family>` groups; the group's family must match the instance's engine version
- Manual snapshots (Create, Describe, Delete, attributes), including final snapshots on delete
- Tags on instances, subnet groups, parameter groups and snapshots
- The endpoint is `localhost` on the instance's port

### RDS real mode (`-docker`)
With `may -docker`, each DB instance is a database you can connect to:
- It runs the official image for its major version (`postgres:16`, `mysql:8.4`, `mariadb:11.4`); the
  instance class limits CPU and memory, and the data lives in a Docker volume
- The port is published on 127.0.0.1 at the instance's `Port` (5432 or 3306 by default); if that port is
  taken, a free one is used and the endpoint reports it. `GET /_may/rds/instances` lists them
- The master user and `DBName` are created on first boot (the MySQL/MariaDB master user gets all
  privileges), and the instance becomes `available` once the server accepts connections
- Parameter group values are passed to the server at start, so changes apply on reboot
  (`ParameterApplyStatus` is `pending-reboot` until then); master password changes take effect at once;
  stop and start keep the data; delete removes the container and its volume. Databases are removed when
  may exits.

## Known gaps

- IAM policies and bucket policies are stored but **never enforced**; SigV4 signatures are not verified.
- State is in memory only and is lost on restart.
- S3 keeps only the latest version of each object (versioning status is stored, but old versions are not).
- SQS: the 60-second `QueueDeletedRecently` and `PurgeQueueInProgress` limits are not enforced, so tests can
  recreate and purge queues freely. Queue policies are stored, not enforced.
- DynamoDB: expired TTL items are not deleted; streams are reported but no stream records are produced
  (there is no DynamoDB Streams API); reserved words are accepted as attribute names in expressions;
  PartiQL, backups, global tables, import/export and Kinesis destinations are not implemented.
  SQS queue and DynamoDB table names are unique across regions.
- CloudWatch Logs: no Insights queries, no metric or subscription delivery.
- EC2: no Elastic IPs, NAT gateways, standalone volumes or ENIs, snapshots, launch templates, custom
  network ACLs or VPC peering; security groups and network ACLs are not enforced. In real mode,
  the metadata service is reached through `AWS_EC2_METADATA_SERVICE_ENDPOINT`, not 169.254.169.254;
  `#cloud-config` user data is not supported; the first boot installs sshd from the internet.
- RDS: no Aurora or DB clusters, read replicas, option groups (beyond the default), Secrets
  Manager-managed passwords, event subscriptions, proxies or automated backups. Snapshots store metadata
  only and cannot be restored. Changes always apply immediately (as with `ApplyImmediately`), and
  parameter groups hold only the values you set (no engine defaults). In real mode, engine major-version
  upgrades are refused, minor versions run the image of their major line, and other containers (such as
  EC2 instances) reach a database at `host.docker.internal:<port>`, not `localhost`.
- Not yet implemented: AssumeRoleWithWebIdentity/SAML, IAM login profiles, MFA, OIDC/SAML providers,
  presigned URL validation, bucket notifications, S3 Select, object lock retention.

Requests for unimplemented operations get a `NotImplemented` or `InvalidAction` error rather than a fake success.

## Testing

```bash
pip install -r test/boto3/requirements.txt   # needs a current boto3 (Python 3.9+)
./test/run.sh     # Go unit tests, boto3 suite, AWS CLI smoke tests, Terraform apply/plan/destroy
```

## License

MIT. See [LICENSE](LICENSE).

May is not affiliated with, endorsed by, or sponsored by Amazon Web Services. AWS and related marks are trademarks of Amazon.com, Inc. or its affiliates.
