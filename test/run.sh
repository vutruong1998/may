#!/usr/bin/env bash
# Builds may, starts a fresh server and runs every check: Go unit tests, the
# boto3 suite, AWS CLI smoke tests, Terraform, and (if Docker is running) the
# EC2 and RDS real-mode suites against a second server started with -docker.
# Requires: go, python3 with a current boto3 (SQS over JSON) and pytest, aws,
# terraform. Set PYTHON to use another interpreter, e.g. a virtualenv's.
set -euo pipefail
cd "$(dirname "$0")/.."
ROOT=$PWD
WORK=$(mktemp -d)
PORT=${MAY_PORT:-4566}
PYTHON=${PYTHON:-python3}
export MAY_ENDPOINT=http://localhost:$PORT
export AWS_ACCESS_KEY_ID=test AWS_SECRET_ACCESS_KEY=test AWS_DEFAULT_REGION=us-east-1 AWS_PAGER=""

go test ./...
go build -o "$WORK/may" ./cmd/may
"$WORK/may" -port "$PORT" -quiet &
PID=$!
trap 'kill $PID 2>/dev/null; rm -rf "$WORK"' EXIT
for _ in $(seq 50); do curl -sf "$MAY_ENDPOINT/_may/health" >/dev/null && break; sleep 0.1; done

echo "== boto3"
"$PYTHON" -m pytest -q -p no:warnings test/boto3

echo "== AWS CLI"
aws() { command aws --endpoint-url "$MAY_ENDPOINT" "$@"; }
mkdir -p "$WORK/up/sub" "$WORK/down"
head -c 20000000 /dev/urandom > "$WORK/up/big.bin"   # > 8 MB: multipart
echo hello > "$WORK/up/a.txt"; echo world > "$WORK/up/sub/b c.txt"
aws s3 mb s3://cli-smoke
aws s3 cp --quiet "$WORK/up/big.bin" s3://cli-smoke/big.bin
aws s3 sync --quiet "$WORK/up" s3://cli-smoke/tree
aws s3 ls --recursive s3://cli-smoke
aws s3 cp --quiet s3://cli-smoke/big.bin "$WORK/down/big.bin"
aws s3 sync --quiet s3://cli-smoke/tree "$WORK/down/tree"
cmp "$WORK/up/big.bin" "$WORK/down/big.bin"
diff -r "$WORK/up" "$WORK/down/tree"
aws s3 rb --force s3://cli-smoke >/dev/null
aws iam create-role --role-name cli-smoke --assume-role-policy-document '{"Version":"2012-10-17","Statement":[]}' >/dev/null
aws iam delete-role --role-name cli-smoke
aws sts get-caller-identity >/dev/null
# SQS: AWS CLI versions before 2.14 use the query protocol, newer ones JSON.
Q=$(aws sqs create-queue --queue-name cli-smoke --query QueueUrl --output text)
aws sqs send-message --queue-url "$Q" --message-body hello \
  --message-attributes '{"k":{"DataType":"String","StringValue":"v"}}' >/dev/null
[ "$(aws sqs receive-message --queue-url "$Q" --message-attribute-names All --query 'Messages[0].Body' --output text)" = hello ]
aws sqs delete-queue --queue-url "$Q"
aws dynamodb create-table --table-name cli-smoke --billing-mode PAY_PER_REQUEST \
  --attribute-definitions AttributeName=id,AttributeType=S --key-schema AttributeName=id,KeyType=HASH >/dev/null
aws dynamodb wait table-exists --table-name cli-smoke
aws dynamodb put-item --table-name cli-smoke --item '{"id":{"S":"a"},"n":{"N":"1"}}'
aws dynamodb update-item --table-name cli-smoke --key '{"id":{"S":"a"}}' \
  --update-expression "SET n = n + :one" --expression-attribute-values '{":one":{"N":"1"}}'
[ "$(aws dynamodb get-item --table-name cli-smoke --key '{"id":{"S":"a"}}' --query Item.n.N --output text)" = 2 ]
aws dynamodb delete-table --table-name cli-smoke >/dev/null
aws logs create-log-group --log-group-name /cli/smoke
aws logs create-log-stream --log-group-name /cli/smoke --log-stream-name s
aws logs put-log-events --log-group-name /cli/smoke --log-stream-name s \
  --log-events "timestamp=$(($(date +%s) * 1000)),message=ERROR smoke" >/dev/null
[ "$(aws logs filter-log-events --log-group-name /cli/smoke --filter-pattern ERROR --query 'events[0].message' --output text)" = "ERROR smoke" ]
aws logs delete-log-group --log-group-name /cli/smoke
AMI=$(aws ec2 describe-images --owners amazon --filters 'Name=name,Values=al2023-ami-*-x86_64' --query 'Images[0].ImageId' --output text)
IID=$(aws ec2 run-instances --image-id "$AMI" --instance-type t3.micro --query 'Instances[0].InstanceId' --output text)
aws ec2 wait instance-running --instance-ids "$IID"
aws ec2 terminate-instances --instance-ids "$IID" >/dev/null
aws ec2 wait instance-terminated --instance-ids "$IID"
# RDS: poll instead of "aws rds wait", which sleeps 30 s between checks.
db_status() { aws rds describe-db-instances --db-instance-identifier cli-smoke --query 'DBInstances[0].DBInstanceStatus' --output text 2>/dev/null; }
aws rds create-db-instance --db-instance-identifier cli-smoke --engine postgres --db-instance-class db.t3.micro \
  --master-username app --master-user-password secret123 --allocated-storage 20 >/dev/null
for _ in $(seq 50); do [ "$(db_status)" = available ] && break; sleep 0.2; done
[ "$(aws rds describe-db-instances --db-instance-identifier cli-smoke --query 'DBInstances[0].Endpoint.Port' --output text)" = 5432 ]
aws rds delete-db-instance --db-instance-identifier cli-smoke --skip-final-snapshot >/dev/null
for _ in $(seq 50); do [ -z "$(db_status)" ] && break; sleep 0.2; done
[ -z "$(db_status)" ]
echo "AWS CLI OK"

echo "== Terraform"
cp test/terraform/*.tf "$WORK/"
(
  cd "$WORK"
  terraform init -input=false -no-color >/dev/null
  terraform apply -auto-approve -input=false -no-color -var "endpoint=$MAY_ENDPOINT" >/dev/null
  terraform plan -detailed-exitcode -input=false -no-color -var "endpoint=$MAY_ENDPOINT" >/dev/null
  terraform destroy -auto-approve -input=false -no-color -var "endpoint=$MAY_ENDPOINT" >/dev/null
)
echo "Terraform OK (apply, no-drift plan, destroy)"

echo "== EC2 and RDS real mode (Docker)"
if [ -z "${MAY_SKIP_DOCKER:-}" ] && docker info >/dev/null 2>&1; then
  DPORT=$((PORT + 1))
  "$WORK/may" -port "$DPORT" -docker -quiet &
  DPID=$!
  trap 'kill $PID $DPID 2>/dev/null; wait $DPID 2>/dev/null; rm -rf "$WORK"' EXIT
  for _ in $(seq 50); do curl -sf "http://localhost:$DPORT/_may/health" >/dev/null && break; sleep 0.1; done
  MAY_DOCKER_ENDPOINT="http://localhost:$DPORT" "$PYTHON" -m pytest -q -p no:warnings test/docker
else
  echo "skipped (Docker not available, or MAY_SKIP_DOCKER is set)"
fi
echo "All checks passed."
