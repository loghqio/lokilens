variable "name" {
  description = "Name prefix for all resources"
  type        = string
  default     = "lokilens"
}

variable "region" {
  description = "AWS region"
  type        = string
}

variable "vpc_id" {
  description = "VPC ID to deploy into"
  type        = string
}

variable "subnet_ids" {
  description = "Subnet IDs for the ECS tasks. Must be private subnets with a NAT gateway for outbound internet (Slack, Gemini, CloudWatch APIs)."
  type        = list(string)
}

# -- Log Backend --

variable "log_backend" {
  description = "Log backend: 'cloudwatch' or 'loki'"
  type        = string
  default     = "cloudwatch"
}

variable "cw_log_groups" {
  description = "Comma-separated CloudWatch log group names to query (required when log_backend = 'cloudwatch')"
  type        = string
  default     = ""
}

# -- Loki (only if log_backend = 'loki') --

variable "loki_url" {
  description = "Loki base URL (required when log_backend = 'loki')"
  type        = string
  default     = ""
}

variable "loki_api_key" {
  description = "Loki API key (only if log_backend = 'loki')"
  type        = string
  default     = ""
  sensitive   = true
}

# -- Slack --

variable "slack_bot_token" {
  description = "Slack bot token (xoxb-...)"
  type        = string
  sensitive   = true
}

variable "slack_app_token" {
  description = "Slack app-level token (xapp-...)"
  type        = string
  sensitive   = true
}

# -- Gemini --

variable "gemini_api_key" {
  description = "Gemini API key"
  type        = string
  sensitive   = true
}

variable "gemini_model" {
  description = "Gemini model name"
  type        = string
  default     = "gemini-2.5-flash"
}

# -- Container --

variable "image" {
  description = "Docker image to deploy"
  type        = string
  default     = "loghqio/lokilens:latest"
}

variable "cpu" {
  description = "Fargate task CPU units (256 = 0.25 vCPU)"
  type        = number
  default     = 256
}

variable "memory" {
  description = "Fargate task memory in MiB"
  type        = number
  default     = 512
}

variable "tags" {
  description = "Tags to apply to all resources"
  type        = map(string)
  default     = {}
}
