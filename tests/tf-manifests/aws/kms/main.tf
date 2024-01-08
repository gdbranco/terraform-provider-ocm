terraform {
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = ">= 4.20.0"
    }
  }
}

provider "aws" {
  region = var.aws_region
}

# Get current account ID
data "aws_caller_identity" "current" {
}

resource "aws_kms_key" "cluster_kms_key" {
  description = "BYOK Test Key for API automation"
  tags = {
    Key         = var.tag_key
    Value       = var.tag_value
    Description = var.tag_description
  }
  deletion_window_in_days = 7
}

locals {
  path = coalesce(var.path, "/")
}

resource "aws_kms_key_policy" "cluster_kms_key_policy" {
  key_id = aws_kms_key.cluster_kms_key.id
  policy = jsonencode({
    Id = var.kms_name
    Statement = [
      {
        Action    = "kms:*"
        Effect    = "Allow"
        Principal = {
          AWS = [
          "arn:aws:iam::${data.aws_caller_identity.current.account_id}:root",
          "arn:aws:iam::${data.aws_caller_identity.current.account_id}:role${local.path}${var.account_role_prefix}-Installer-Role",
          "arn:aws:iam::${data.aws_caller_identity.current.account_id}:role${local.path}${var.account_role_prefix}-Support-Role",
          "arn:aws:iam::${data.aws_caller_identity.current.account_id}:role${local.path}${var.account_role_prefix}-ControlPlane-Role",
          "arn:aws:iam::${data.aws_caller_identity.current.account_id}:role${local.path}${var.account_role_prefix}-Worker-Role"
          ]
        }
        Resource  = "*"
        Sid       = "Enable IAM User Permissions"
      },
    ]
    Version = "2012-10-17"
  })

}