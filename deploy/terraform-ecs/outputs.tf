output "cluster_name" {
  description = "ECS cluster name"
  value       = aws_ecs_cluster.this.name
}

output "service_name" {
  description = "ECS service name"
  value       = aws_ecs_service.this.name
}

output "task_role_arn" {
  description = "IAM role ARN used by the running container"
  value       = aws_iam_role.task.arn
}

output "log_group" {
  description = "CloudWatch log group for LokiLens container logs"
  value       = aws_cloudwatch_log_group.app.name
}

output "security_group_id" {
  description = "Security group ID for the ECS tasks"
  value       = aws_security_group.task.id
}
