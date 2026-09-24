# IAM for the control host: the luxd instance role. Runners get no
# instance profile (see runners.tf): Fedora CoreOS has no SSM agent, is
# reached through lux itself, and never holds S3 credentials, so luxd's
# role needs no iam:PassRole either — RunInstances launches the runner
# template as-is, with no IAM role to pass.

data "aws_partition" "current" {}

data "aws_region" "current" {}

# --- control host ---------------------------------------------------------

resource "aws_iam_role" "control" {
  name = "${var.name}-control"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { Service = "ec2.amazonaws.com" }
      Action    = "sts:AssumeRole"
    }]
  })

  tags = var.tags
}

resource "aws_iam_instance_profile" "control" {
  name = "${var.name}-control"
  role = aws_iam_role.control.name

  tags = var.tags
}

# Shell access to the control host is SSM Session Manager only (no SSH, no
# key pairs).
resource "aws_iam_role_policy_attachment" "control_ssm" {
  role       = aws_iam_role.control.name
  policy_arn = "arn:${data.aws_partition.current.partition}:iam::aws:policy/AmazonSSMManagedInstanceCore"
}

resource "aws_iam_role_policy" "control_luxd" {
  name = "${var.name}-luxd"
  role = aws_iam_role.control.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = concat([
      {
        Sid    = "RunRunnerInstances"
        Effect = "Allow"
        Action = "ec2:RunInstances"
        # RunInstances is authorized per resource type touched by the
        # call; scoping each type to the runner pools' own launch
        # templates, subnets and security group stops luxd's role (if a
        # credential leaked) from launching arbitrary instances.
        Resource = concat(
          [for lt in aws_launch_template.runner : "arn:${data.aws_partition.current.partition}:ec2:${var.region}:${data.aws_caller_identity.current.account_id}:launch-template/${lt.id}"],
          [for s in aws_subnet.public : "arn:${data.aws_partition.current.partition}:ec2:${var.region}:${data.aws_caller_identity.current.account_id}:subnet/${s.id}"],
          [
            "arn:${data.aws_partition.current.partition}:ec2:${var.region}:${data.aws_caller_identity.current.account_id}:instance/*",
            "arn:${data.aws_partition.current.partition}:ec2:${var.region}:${data.aws_caller_identity.current.account_id}:network-interface/*",
            "arn:${data.aws_partition.current.partition}:ec2:${var.region}:${data.aws_caller_identity.current.account_id}:volume/*",
            "arn:${data.aws_partition.current.partition}:ec2:${var.region}::image/*",
            "arn:${data.aws_partition.current.partition}:ec2:${var.region}:${data.aws_caller_identity.current.account_id}:security-group/${aws_security_group.runner.id}",
          ]
        )
      },
      {
        Sid      = "TagOnCreate"
        Effect   = "Allow"
        Action   = "ec2:CreateTags"
        Resource = "arn:${data.aws_partition.current.partition}:ec2:${var.region}:${data.aws_caller_identity.current.account_id}:instance/*"
        Condition = {
          StringEquals = { "ec2:CreateAction" = "RunInstances" }
        }
      },
      {
        Sid      = "TerminateManagedInstances"
        Effect   = "Allow"
        Action   = "ec2:TerminateInstances"
        Resource = "arn:${data.aws_partition.current.partition}:ec2:${var.region}:${data.aws_caller_identity.current.account_id}:instance/*"
        Condition = {
          StringEquals = { "ec2:ResourceTag/lux:managed" = "true" }
        }
      },
      {
        Sid      = "DescribeInstances"
        Effect   = "Allow"
        Action   = "ec2:DescribeInstances"
        Resource = "*" # DescribeInstances does not support resource-level permissions.
      },
      {
        Sid    = "Blobs"
        Effect = "Allow"
        Action = [
          "s3:GetObject",
          "s3:PutObject",
          "s3:DeleteObject",
          "s3:ListBucket",
        ]
        Resource = [
          aws_s3_bucket.blobs.arn,
          "${aws_s3_bucket.blobs.arn}/*",
        ]
      },
      {
        Sid    = "PgBackups"
        Effect = "Allow"
        Action = [
          "s3:PutObject",
          "s3:GetObject",
          "s3:ListBucket",
        ]
        Resource = [
          aws_s3_bucket.pg_backups.arn,
          "${aws_s3_bucket.pg_backups.arn}/*",
        ]
      },
      {
        Sid      = "ReadOwnParameters"
        Effect   = "Allow"
        Action   = ["ssm:GetParameter", "ssm:GetParameters"]
        Resource = "arn:${data.aws_partition.current.partition}:ssm:${var.region}:${data.aws_caller_identity.current.account_id}:parameter${local.ssm_prefix}/*"
      },
      {
        Sid    = "WriteOwnSecureStrings"
        Effect = "Allow"
        # The box (not Terraform) generates the Postgres owner/app
        # passwords at first boot and may keep them here instead of a
        # root-only file (docs/luxd.example.toml notes secrets never
        # belong in Terraform state).
        Action   = ["ssm:PutParameter"]
        Resource = "arn:${data.aws_partition.current.partition}:ssm:${var.region}:${data.aws_caller_identity.current.account_id}:parameter${local.ssm_prefix}/*"
      },
      {
        Sid      = "DecryptSSMSecureStrings"
        Effect   = "Allow"
        Action   = "kms:Decrypt"
        Resource = "arn:${data.aws_partition.current.partition}:kms:${var.region}:${data.aws_caller_identity.current.account_id}:alias/aws/ssm"
      },
      ],
      # GenerateDataKey is also needed when the box itself writes SecureStrings
      # (PutParameter with SecureString type encrypts client-side via KMS).
      [{
        Sid      = "EncryptSSMSecureStrings"
        Effect   = "Allow"
        Action   = "kms:GenerateDataKey"
        Resource = "arn:${data.aws_partition.current.partition}:kms:${var.region}:${data.aws_caller_identity.current.account_id}:alias/aws/ssm"
    }])
  })
}
