# Phase 5: RDS. A PostgreSQL instance in the VPC from compute.tf, with a
# subnet group, a custom parameter group and a security group, plus a MySQL
# instance in the default VPC and a manual snapshot.

resource "aws_db_subnet_group" "main" {
  name       = "may-tf-db"
  subnet_ids = [aws_subnet.public.id, aws_subnet.private.id]
  tags       = { Project = "may" }
}

resource "aws_db_parameter_group" "pg" {
  name   = "may-tf-pg16"
  family = "postgres16"

  parameter {
    name  = "log_min_duration_statement"
    value = "500"
  }

  parameter {
    name         = "max_connections"
    value        = "50"
    apply_method = "pending-reboot"
  }
}

resource "aws_security_group" "db" {
  name        = "may-tf-db"
  description = "PostgreSQL from the web servers"
  vpc_id      = aws_vpc.main.id

  ingress {
    from_port       = 5432
    to_port         = 5432
    protocol        = "tcp"
    security_groups = [aws_security_group.web.id]
  }
}

resource "aws_db_instance" "app" {
  identifier              = "may-tf-app"
  engine                  = "postgres"
  engine_version          = "16"
  instance_class          = "db.t3.micro"
  allocated_storage       = 20
  max_allocated_storage   = 100
  storage_type            = "gp3"
  db_name                 = "app"
  username                = "app"
  password                = "may-tf-secret"
  db_subnet_group_name    = aws_db_subnet_group.main.name
  parameter_group_name    = aws_db_parameter_group.pg.name
  vpc_security_group_ids  = [aws_security_group.db.id]
  backup_retention_period = 7
  storage_encrypted       = true
  skip_final_snapshot     = true
  tags                    = { Project = "may" }
}

resource "aws_db_instance" "reports" {
  identifier          = "may-tf-reports"
  engine              = "mysql"
  instance_class      = "db.t3.micro"
  allocated_storage   = 20
  username            = "admin"
  password            = "may-tf-secret"
  port                = 3307
  skip_final_snapshot = true
}

resource "aws_db_snapshot" "app" {
  db_instance_identifier = aws_db_instance.app.identifier
  db_snapshot_identifier = "may-tf-app-snap"
}

output "db_endpoint" {
  value = aws_db_instance.app.endpoint
}
