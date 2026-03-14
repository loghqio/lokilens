terraform {
  required_version = ">= 1.5"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 5.0"
    }
  }
}

provider "aws" {
  region = var.region
}

data "aws_caller_identity" "current" {}

locals {
  account_id    = data.aws_caller_identity.current.account_id
  is_cloudwatch = var.log_backend == "cloudwatch"
  is_loki       = var.log_backend == "loki"

  # Build env vars based on backend — no Loki vars for CloudWatch, no CW vars for Loki
  backend_env = local.is_cloudwatch ? [
    { name = "LOG_BACKEND", value = "cloudwatch" },
    { name = "AWS_REGION", value = var.region },
    { name = "CW_LOG_GROUPS", value = var.cw_log_groups },
  ] : [
    { name = "LOG_BACKEND", value = "loki" },
    { name = "LOKI_BASE_URL", value = var.loki_url },
  ]

  # Secrets: always Slack + Gemini, plus Loki API key only if Loki backend
  base_secrets = [
    { name = "SLACK_BOT_TOKEN", valueFrom = aws_ssm_parameter.slack_bot_token.arn },
    { name = "SLACK_APP_TOKEN", valueFrom = aws_ssm_parameter.slack_app_token.arn },
    { name = "GEMINI_API_KEY", valueFrom = aws_ssm_parameter.gemini_api_key.arn },
  ]
  loki_secrets = local.is_loki && var.loki_api_key != "" ? [
    { name = "LOKI_API_KEY", valueFrom = aws_ssm_parameter.loki_api_key[0].arn },
  ] : []
  all_secrets = concat(local.base_secrets, local.loki_secrets)

  # SSM ARNs for IAM policy
  ssm_arns = concat(
    [
      aws_ssm_parameter.slack_bot_token.arn,
      aws_ssm_parameter.slack_app_token.arn,
      aws_ssm_parameter.gemini_api_key.arn,
    ],
    local.is_loki && var.loki_api_key != "" ? [aws_ssm_parameter.loki_api_key[0].arn] : [],
  )
}

# -------------------------------------------------------------------
# IAM — CloudWatch Logs read-only (only for CloudWatch backend)
# -------------------------------------------------------------------

resource "aws_iam_policy" "cloudwatch_logs" {
  count  = local.is_cloudwatch ? 1 : 0
  name   = "${var.name}-cloudwatch-logs"
  tags   = var.tags

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Action = [
        "logs:StartQuery",
        "logs:GetQueryResults",
        "logs:DescribeLogGroups",
        "logs:FilterLogEvents",
      ]
      Resource = "arn:aws:logs:${var.region}:${local.account_id}:log-group:*"
    }]
  })
}

data "aws_iam_policy_document" "ecs_assume" {
  statement {
    effect  = "Allow"
    actions = ["sts:AssumeRole"]
    principals {
      type        = "Service"
      identifiers = ["ecs-tasks.amazonaws.com"]
    }
  }
}

# Task execution role (pulls images, writes logs, reads SSM)
resource "aws_iam_role" "execution" {
  name               = "${var.name}-execution"
  assume_role_policy = data.aws_iam_policy_document.ecs_assume.json
  tags               = var.tags
}

resource "aws_iam_role_policy_attachment" "execution" {
  role       = aws_iam_role.execution.name
  policy_arn = "arn:aws:iam::aws:policy/service-role/AmazonECSTaskExecutionRolePolicy"
}

# Task role (what the container can do at runtime)
resource "aws_iam_role" "task" {
  name               = "${var.name}-task"
  assume_role_policy = data.aws_iam_policy_document.ecs_assume.json
  tags               = var.tags
}

resource "aws_iam_role_policy_attachment" "task_cloudwatch" {
  count      = local.is_cloudwatch ? 1 : 0
  role       = aws_iam_role.task.name
  policy_arn = aws_iam_policy.cloudwatch_logs[0].arn
}

# -------------------------------------------------------------------
# ECS Cluster
# -------------------------------------------------------------------

resource "aws_ecs_cluster" "this" {
  name = var.name
  tags = var.tags

  setting {
    name  = "containerInsights"
    value = "enabled"
  }
}

# -------------------------------------------------------------------
# CloudWatch Log Group for the LokiLens container itself
# -------------------------------------------------------------------

resource "aws_cloudwatch_log_group" "app" {
  name              = "/ecs/${var.name}"
  retention_in_days = 14
  tags              = var.tags
}

# -------------------------------------------------------------------
# Security Group — outbound only (Socket Mode, no inbound needed)
# -------------------------------------------------------------------

resource "aws_security_group" "task" {
  name_prefix = "${var.name}-"
  vpc_id      = var.vpc_id
  tags        = var.tags

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}

# -------------------------------------------------------------------
# ECS Task Definition
# -------------------------------------------------------------------

resource "aws_ecs_task_definition" "this" {
  family                   = var.name
  requires_compatibilities = ["FARGATE"]
  network_mode             = "awsvpc"
  cpu                      = var.cpu
  memory                   = var.memory
  execution_role_arn       = aws_iam_role.execution.arn
  task_role_arn            = aws_iam_role.task.arn
  tags                     = var.tags

  container_definitions = jsonencode([
    {
      name      = var.name
      image     = var.image
      essential = true

      environment = concat(local.backend_env, [
        { name = "GEMINI_MODEL", value = var.gemini_model },
        { name = "HEALTH_ADDR", value = ":8080" },
        { name = "LOG_LEVEL", value = "info" },
      ])

      secrets = local.all_secrets

      portMappings = [
        { containerPort = 8080, protocol = "tcp" }
      ]

      healthCheck = {
        command     = ["CMD-SHELL", "wget -q --spider http://localhost:8080/healthz || exit 1"]
        interval    = 30
        timeout     = 5
        retries     = 3
        startPeriod = 10
      }

      logConfiguration = {
        logDriver = "awslogs"
        options = {
          "awslogs-group"         = aws_cloudwatch_log_group.app.name
          "awslogs-region"        = var.region
          "awslogs-stream-prefix" = "ecs"
        }
      }
    }
  ])
}

# -------------------------------------------------------------------
# SSM Parameter Store — secrets (encrypted at rest)
# -------------------------------------------------------------------

resource "aws_ssm_parameter" "slack_bot_token" {
  name  = "/${var.name}/slack-bot-token"
  type  = "SecureString"
  value = var.slack_bot_token
  tags  = var.tags
}

resource "aws_ssm_parameter" "slack_app_token" {
  name  = "/${var.name}/slack-app-token"
  type  = "SecureString"
  value = var.slack_app_token
  tags  = var.tags
}

resource "aws_ssm_parameter" "gemini_api_key" {
  name  = "/${var.name}/gemini-api-key"
  type  = "SecureString"
  value = var.gemini_api_key
  tags  = var.tags
}

# Only created when using Loki backend with an API key
resource "aws_ssm_parameter" "loki_api_key" {
  count = local.is_loki && var.loki_api_key != "" ? 1 : 0
  name  = "/${var.name}/loki-api-key"
  type  = "SecureString"
  value = var.loki_api_key
  tags  = var.tags
}

# -------------------------------------------------------------------
# ECS Service
# -------------------------------------------------------------------

resource "aws_ecs_service" "this" {
  name            = var.name
  cluster         = aws_ecs_cluster.this.id
  task_definition = aws_ecs_task_definition.this.arn
  desired_count   = 1
  launch_type     = "FARGATE"
  tags            = var.tags

  lifecycle {
    precondition {
      condition     = var.log_backend != "cloudwatch" || var.cw_log_groups != ""
      error_message = "cw_log_groups is required when log_backend is 'cloudwatch'."
    }
    precondition {
      condition     = var.log_backend != "loki" || var.loki_url != ""
      error_message = "loki_url is required when log_backend is 'loki'."
    }
  }

  network_configuration {
    subnets          = var.subnet_ids
    security_groups  = [aws_security_group.task.id]
    assign_public_ip = false
  }

  deployment_circuit_breaker {
    enable   = true
    rollback = true
  }
}

# -------------------------------------------------------------------
# IAM — allow execution role to read SSM secrets
# -------------------------------------------------------------------

resource "aws_iam_role_policy" "execution_ssm" {
  name = "${var.name}-ssm-read"
  role = aws_iam_role.execution.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect   = "Allow"
      Action   = ["ssm:GetParameters", "ssm:GetParameter"]
      Resource = local.ssm_arns
    }]
  })
}
