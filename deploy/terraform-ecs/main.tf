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
  account_id = data.aws_caller_identity.current.account_id
}

# -------------------------------------------------------------------
# IAM — CloudWatch Logs read-only access for the task
# -------------------------------------------------------------------

data "aws_iam_policy_document" "cloudwatch_logs" {
  statement {
    effect = "Allow"
    actions = [
      "logs:StartQuery",
      "logs:GetQueryResults",
      "logs:DescribeLogGroups",
      "logs:FilterLogEvents",
    ]
    resources = ["arn:aws:logs:${var.region}:${local.account_id}:log-group:*"]
  }
}

resource "aws_iam_policy" "cloudwatch_logs" {
  name   = "${var.name}-cloudwatch-logs"
  policy = data.aws_iam_policy_document.cloudwatch_logs.json
  tags   = var.tags
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

# Task execution role (pulls images, writes logs)
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
  role       = aws_iam_role.task.name
  policy_arn = aws_iam_policy.cloudwatch_logs.arn
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

      environment = [
        { name = "LOG_BACKEND", value = var.log_backend },
        { name = "AWS_REGION", value = var.region },
        { name = "CW_LOG_GROUPS", value = var.cw_log_groups },
        { name = "LOKI_BASE_URL", value = var.loki_url },
        { name = "GEMINI_MODEL", value = var.gemini_model },
        { name = "HEALTH_ADDR", value = ":8080" },
        { name = "LOG_LEVEL", value = "info" },
      ]

      secrets = [
        { name = "SLACK_BOT_TOKEN", valueFrom = aws_ssm_parameter.slack_bot_token.arn },
        { name = "SLACK_APP_TOKEN", valueFrom = aws_ssm_parameter.slack_app_token.arn },
        { name = "GEMINI_API_KEY", valueFrom = aws_ssm_parameter.gemini_api_key.arn },
        { name = "LOKI_API_KEY", valueFrom = aws_ssm_parameter.loki_api_key.arn },
      ]

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

resource "aws_ssm_parameter" "loki_api_key" {
  name  = "/${var.name}/loki-api-key"
  type  = "SecureString"
  value = var.loki_api_key != "" ? var.loki_api_key : "unused"
  tags  = var.tags
}

# -------------------------------------------------------------------
# ECS Service — runs the task with auto-restart
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

data "aws_iam_policy_document" "ssm_read" {
  statement {
    effect = "Allow"
    actions = [
      "ssm:GetParameters",
      "ssm:GetParameter",
    ]
    resources = [
      aws_ssm_parameter.slack_bot_token.arn,
      aws_ssm_parameter.slack_app_token.arn,
      aws_ssm_parameter.gemini_api_key.arn,
      aws_ssm_parameter.loki_api_key.arn,
    ]
  }
}

resource "aws_iam_role_policy" "execution_ssm" {
  name   = "${var.name}-ssm-read"
  role   = aws_iam_role.execution.id
  policy = data.aws_iam_policy_document.ssm_read.json
}
