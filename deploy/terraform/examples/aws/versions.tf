terraform {
  required_version = ">= 1.10"

  # Partial config: bootstrap the bucket first (see deploy/terraform/README.md),
  # then run `terraform init -backend-config=backend.hcl` (or pass
  # -backend-config values directly) naming the bucket, key and region. S3
  # native locking (use_lockfile = true) needs no DynamoDB table.
  backend "s3" {
    use_lockfile = true
  }

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 6.0"
    }
    cloudflare = {
      source  = "cloudflare/cloudflare"
      version = "~> 5.0"
    }
  }
}

provider "aws" {
  region = var.region
}

provider "cloudflare" {}
