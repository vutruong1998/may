# Phases 3-4: CloudWatch Logs and EC2 (API only). An instance in a custom VPC
# with a public subnet, security groups, an imported key pair, the instance
# profile from main.tf, and an AMI found by name like on AWS.

resource "aws_cloudwatch_log_group" "app" {
  name              = "/may/tf/app"
  retention_in_days = 14
  tags              = { Project = "may" }
}

resource "aws_cloudwatch_log_stream" "web" {
  name           = "web-1"
  log_group_name = aws_cloudwatch_log_group.app.name
}

resource "aws_cloudwatch_log_metric_filter" "errors" {
  name           = "errors"
  log_group_name = aws_cloudwatch_log_group.app.name
  pattern        = "ERROR"

  metric_transformation {
    name      = "ErrorCount"
    namespace = "May/App"
    value     = "1"
  }
}

resource "aws_vpc" "main" {
  cidr_block           = "10.42.0.0/16"
  enable_dns_hostnames = true
  tags                 = { Name = "may-tf" }
}

resource "aws_subnet" "public" {
  vpc_id                  = aws_vpc.main.id
  cidr_block              = "10.42.1.0/24"
  availability_zone       = "us-east-1a"
  map_public_ip_on_launch = true
  tags                    = { Name = "may-tf-public" }
}

resource "aws_subnet" "private" {
  vpc_id            = aws_vpc.main.id
  cidr_block        = "10.42.2.0/24"
  availability_zone = "us-east-1b"
}

resource "aws_internet_gateway" "main" {
  vpc_id = aws_vpc.main.id
}

resource "aws_route_table" "public" {
  vpc_id = aws_vpc.main.id

  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.main.id
  }
}

resource "aws_route_table_association" "public" {
  subnet_id      = aws_subnet.public.id
  route_table_id = aws_route_table.public.id
}

resource "aws_security_group" "web" {
  name        = "may-tf-web"
  description = "Web servers"
  vpc_id      = aws_vpc.main.id

  ingress {
    description = "HTTPS"
    from_port   = 443
    to_port     = 443
    protocol    = "tcp"
    cidr_blocks = ["0.0.0.0/0"]
  }

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}

# Rules managed as separate resources need a group without inline rules:
# mixing both styles makes Terraform fight itself, on AWS too.
resource "aws_security_group" "ssh" {
  name        = "may-tf-ssh"
  description = "SSH access"
  vpc_id      = aws_vpc.main.id
}

resource "aws_vpc_security_group_ingress_rule" "ssh" {
  security_group_id = aws_security_group.ssh.id
  cidr_ipv4         = "10.0.0.0/8"
  from_port         = 22
  to_port           = 22
  ip_protocol       = "tcp"
  description       = "SSH from the office"
}

resource "aws_key_pair" "deployer" {
  key_name   = "may-tf-deployer"
  public_key = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl deployer"
}

data "aws_ami" "ubuntu" {
  most_recent = true
  owners      = ["099720109477"]

  filter {
    name   = "name"
    values = ["ubuntu/images/hvm-ssd-gp3/ubuntu-noble-24.04-amd64-server-*"]
  }
}

resource "aws_instance" "web" {
  ami                    = data.aws_ami.ubuntu.id
  instance_type          = "t3.micro"
  subnet_id              = aws_subnet.public.id
  vpc_security_group_ids = [aws_security_group.web.id, aws_security_group.ssh.id]
  key_name               = aws_key_pair.deployer.key_name
  iam_instance_profile   = aws_iam_instance_profile.app.name
  user_data              = "#!/bin/sh\necho hello from may > /tmp/hello\n"

  root_block_device {
    volume_size = 16
    volume_type = "gp3"
  }

  tags = { Name = "may-tf-web" }
}

output "instance_private_ip" {
  value = aws_instance.web.private_ip
}

output "instance_public_ip" {
  value = aws_instance.web.public_ip
}
