package iam

import "strings"

// Built-in AWS managed policies. Tools such as Terraform attach these by ARN,
// so they must exist. The documents are simplified: may never enforces them.

const (
	allowAll  = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"*","Resource":"*"}]}`
	readOnly  = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["*:Describe*","*:Get*","*:List*"],"Resource":"*"}]}`
	logsBasic = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["logs:CreateLogGroup","logs:CreateLogStream","logs:PutLogEvents"],"Resource":"*"}]}`
)

func service(actions ...string) string {
	return `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["` +
		strings.Join(actions, `","`) + `"],"Resource":"*"}]}`
}

var managedPolicies = []struct{ path, name, doc string }{
	{"/", "AdministratorAccess", allowAll},
	{"/job-function/", "ViewOnlyAccess", readOnly},
	{"/", "ReadOnlyAccess", readOnly},
	{"/", "PowerUserAccess", `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","NotAction":["iam:*","organizations:*","account:*"],"Resource":"*"}]}`},
	{"/", "IAMFullAccess", service("iam:*")},
	{"/", "AmazonS3FullAccess", service("s3:*", "s3-object-lambda:*")},
	{"/", "AmazonS3ReadOnlyAccess", service("s3:Get*", "s3:List*")},
	{"/", "AmazonDynamoDBFullAccess", service("dynamodb:*")},
	{"/", "AmazonSQSFullAccess", service("sqs:*")},
	{"/", "AmazonSNSFullAccess", service("sns:*")},
	{"/", "AmazonEC2FullAccess", service("ec2:*", "elasticloadbalancing:*", "autoscaling:*", "cloudwatch:*")},
	{"/", "AmazonEC2ReadOnlyAccess", service("ec2:Describe*")},
	{"/", "AmazonSSMManagedInstanceCore", service("ssm:*", "ssmmessages:*", "ec2messages:*")},
	{"/", "CloudWatchLogsFullAccess", service("logs:*")},
	{"/", "CloudWatchAgentServerPolicy", service("cloudwatch:PutMetricData", "logs:*", "ssm:GetParameter")},
	{"/", "SecretsManagerReadWrite", service("secretsmanager:*")},
	{"/", "AWSLambda_FullAccess", service("lambda:*")},
	{"/", "AWSLambdaExecute", service("logs:*", "s3:GetObject", "s3:PutObject")},
	{"/service-role/", "AWSLambdaBasicExecutionRole", logsBasic},
	{"/service-role/", "AWSLambdaVPCAccessExecutionRole", service("logs:CreateLogGroup", "logs:CreateLogStream", "logs:PutLogEvents", "ec2:CreateNetworkInterface", "ec2:DescribeNetworkInterfaces", "ec2:DeleteNetworkInterface")},
	{"/service-role/", "AWSLambdaSQSQueueExecutionRole", service("sqs:ReceiveMessage", "sqs:DeleteMessage", "sqs:GetQueueAttributes", "logs:CreateLogGroup", "logs:CreateLogStream", "logs:PutLogEvents")},
	{"/service-role/", "AWSLambdaDynamoDBExecutionRole", service("dynamodb:DescribeStream", "dynamodb:GetRecords", "dynamodb:GetShardIterator", "dynamodb:ListStreams", "logs:CreateLogGroup", "logs:CreateLogStream", "logs:PutLogEvents")},
	{"/service-role/", "AmazonECSTaskExecutionRolePolicy", service("ecr:GetAuthorizationToken", "ecr:BatchCheckLayerAvailability", "ecr:GetDownloadUrlForLayer", "ecr:BatchGetImage", "logs:CreateLogStream", "logs:PutLogEvents")},
	{"/", "AmazonEKSClusterPolicy", service("ec2:*", "elasticloadbalancing:*", "iam:CreateServiceLinkedRole", "kms:DescribeKey")},
	{"/", "AmazonEKSWorkerNodePolicy", service("ec2:Describe*", "eks:DescribeCluster")},
	{"/", "AmazonEKS_CNI_Policy", service("ec2:*NetworkInterface*", "ec2:Describe*", "ec2:CreateTags")},
	{"/", "AmazonEC2ContainerRegistryReadOnly", service("ecr:Get*", "ecr:Describe*", "ecr:List*", "ecr:BatchGetImage", "ecr:BatchCheckLayerAvailability")},
}

func (s *Service) seedManagedPolicies() {
	for _, m := range managedPolicies {
		s.addPolicy(m.name, m.path, m.doc, "", true, nil)
	}
}
