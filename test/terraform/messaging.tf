# Phase 2: SQS queues (standard with a dead-letter queue, FIFO, queue policy)
# and DynamoDB tables (on-demand with GSI/LSI, TTL, PITR and a stream;
# provisioned with a GSI) plus a seeded item.

resource "aws_sqs_queue" "dlq" {
  name                      = "may-tf-jobs-dlq"
  message_retention_seconds = 1209600
}

resource "aws_sqs_queue" "jobs" {
  name                       = "may-tf-jobs"
  visibility_timeout_seconds = 60
  receive_wait_time_seconds  = 10
  delay_seconds              = 5
  max_message_size           = 131072
  redrive_policy = jsonencode({
    deadLetterTargetArn = aws_sqs_queue.dlq.arn
    maxReceiveCount     = 5
  })
  tags = { Project = "may" }
}

resource "aws_sqs_queue_redrive_allow_policy" "dlq" {
  queue_url = aws_sqs_queue.dlq.id
  redrive_allow_policy = jsonencode({
    redrivePermission = "byQueue"
    sourceQueueArns   = [aws_sqs_queue.jobs.arn]
  })
}

resource "aws_sqs_queue_policy" "jobs" {
  queue_url = aws_sqs_queue.jobs.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { AWS = aws_iam_role.app.arn }
      Action    = ["sqs:SendMessage", "sqs:ReceiveMessage"]
      Resource  = aws_sqs_queue.jobs.arn
    }]
  })
}

resource "aws_sqs_queue" "events" {
  name                        = "may-tf-events.fifo"
  fifo_queue                  = true
  content_based_deduplication = true
  deduplication_scope         = "messageGroup"
  fifo_throughput_limit       = "perMessageGroupId"
}

resource "aws_dynamodb_table" "orders" {
  name             = "may-tf-orders"
  billing_mode     = "PAY_PER_REQUEST"
  hash_key         = "pk"
  range_key        = "sk"
  stream_enabled   = true
  stream_view_type = "NEW_AND_OLD_IMAGES"

  attribute {
    name = "pk"
    type = "S"
  }
  attribute {
    name = "sk"
    type = "S"
  }
  attribute {
    name = "gsi1pk"
    type = "S"
  }
  attribute {
    name = "created"
    type = "N"
  }

  global_secondary_index {
    name               = "gsi1"
    hash_key           = "gsi1pk"
    range_key          = "sk"
    projection_type    = "INCLUDE"
    non_key_attributes = ["status"]
  }

  local_secondary_index {
    name            = "by-created"
    range_key       = "created"
    projection_type = "KEYS_ONLY"
  }

  ttl {
    attribute_name = "expires_at"
    enabled        = true
  }

  point_in_time_recovery {
    enabled = true
  }

  tags = { Project = "may" }
}

resource "aws_dynamodb_table" "sessions" {
  name           = "may-tf-sessions"
  billing_mode   = "PROVISIONED"
  read_capacity  = 5
  write_capacity = 5
  hash_key       = "id"

  attribute {
    name = "id"
    type = "S"
  }
  attribute {
    name = "user"
    type = "S"
  }

  global_secondary_index {
    name            = "by-user"
    hash_key        = "user"
    projection_type = "ALL"
    read_capacity   = 2
    write_capacity  = 2
  }
}

resource "aws_dynamodb_table_item" "order" {
  table_name = aws_dynamodb_table.orders.name
  hash_key   = aws_dynamodb_table.orders.hash_key
  range_key  = aws_dynamodb_table.orders.range_key
  item = jsonencode({
    pk      = { S = "order#1" }
    sk      = { S = "meta" }
    gsi1pk  = { S = "customer#7" }
    created = { N = "1760000000" }
    status  = { S = "new" }
    lines   = { L = [{ M = { sku = { S = "a" }, qty = { N = "2" } } }] }
  })
}

output "jobs_queue_url" {
  value = aws_sqs_queue.jobs.id
}

output "orders_stream_arn" {
  value = aws_dynamodb_table.orders.stream_arn
}
