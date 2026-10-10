# Definition-of-done check for Phase 1 (this file), Phase 2 (messaging.tf),
# Phases 3-4 (compute.tf) and Phase 5 RDS (database.tf).
# With may running on :4566:
#
#   terraform init && terraform apply -auto-approve && terraform plan -detailed-exitcode && terraform destroy -auto-approve
#
# The plan step must report no changes: Terraform reads back every attribute.

terraform {
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }
}

variable "endpoint" {
  default = "http://localhost:4566"
}

provider "aws" {
  region                      = "us-east-1"
  access_key                  = "test"
  secret_key                  = "test"
  skip_credentials_validation = true
  skip_requesting_account_id  = true
  skip_metadata_api_check     = true
  s3_use_path_style           = true

  endpoints {
    dynamodb = var.endpoint
    ec2      = var.endpoint
    iam      = var.endpoint
    logs     = var.endpoint
    rds      = var.endpoint
    s3       = var.endpoint
    sqs      = var.endpoint
    sts      = var.endpoint
  }
}

# S3 bucket with tags, versioning and a bucket policy.

resource "aws_s3_bucket" "data" {
  bucket = "may-tf-data"
  tags = {
    Environment = "test"
    Project     = "may"
  }
}

resource "aws_s3_bucket_versioning" "data" {
  bucket = aws_s3_bucket.data.id
  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_policy" "data" {
  bucket = aws_s3_bucket.data.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { AWS = aws_iam_role.app.arn }
      Action    = ["s3:GetObject"]
      Resource  = "${aws_s3_bucket.data.arn}/*"
    }]
  })
}

resource "aws_s3_object" "readme" {
  bucket       = aws_s3_bucket.data.id
  key          = "docs/readme.txt"
  content      = "hello from terraform"
  content_type = "text/plain"
}

# IAM role with an assume-role policy, an attached AWS managed policy and an
# inline policy.

resource "aws_iam_role" "app" {
  name = "may-tf-app"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "ec2.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })
  tags = { Project = "may" }
}

resource "aws_iam_role_policy_attachment" "app_s3" {
  role       = aws_iam_role.app.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonS3ReadOnlyAccess"
}

resource "aws_iam_role_policy" "app_inline" {
  name = "logs"
  role = aws_iam_role.app.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect   = "Allow"
      Action   = ["logs:CreateLogStream", "logs:PutLogEvents"]
      Resource = "*"
    }]
  })
}

resource "aws_iam_policy" "custom" {
  name        = "may-tf-custom"
  description = "Customer managed policy"
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect   = "Allow"
      Action   = ["s3:ListBucket"]
      Resource = aws_s3_bucket.data.arn
    }]
  })
}

resource "aws_iam_role_policy_attachment" "app_custom" {
  role       = aws_iam_role.app.name
  policy_arn = aws_iam_policy.custom.arn
}

# Instance profile containing the role.

resource "aws_iam_instance_profile" "app" {
  name = "may-tf-app"
  role = aws_iam_role.app.name
}

# IAM user with an access key, and a group.

resource "aws_iam_user" "ci" {
  name = "may-tf-ci"
  path = "/ci/"
  tags = { Project = "may" }
}

resource "aws_iam_access_key" "ci" {
  user = aws_iam_user.ci.name
}

resource "aws_iam_group" "devs" {
  name = "may-tf-devs"
}

resource "aws_iam_group_membership" "devs" {
  name  = "may-tf-devs-membership"
  group = aws_iam_group.devs.name
  users = [aws_iam_user.ci.name]
}

resource "aws_iam_group_policy_attachment" "devs" {
  group      = aws_iam_group.devs.name
  policy_arn = "arn:aws:iam::aws:policy/ReadOnlyAccess"
}

data "aws_caller_identity" "current" {}

output "account_id" {
  value = data.aws_caller_identity.current.account_id
}

output "access_key_id" {
  value = aws_iam_access_key.ci.id
}
