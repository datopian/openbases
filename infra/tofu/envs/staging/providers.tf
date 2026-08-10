# Root module for the staging environment.
#
# Credentials come from the environment (HCLOUD_TOKEN, CLOUDFLARE_API_TOKEN) or
# from the deployment pipeline. They are never written here and never committed.

terraform {
  required_version = ">= 1.9.0"

  required_providers {
    hcloud = {
      source  = "hetznercloud/hcloud"
      version = "~> 1.51"
    }
    cloudflare = {
      source  = "cloudflare/cloudflare"
      version = "~> 5.0"
    }
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
    }
  }

  # State encryption is configured through the TF_ENCRYPTION environment
  # variable rather than a block here.
  #
  # Creating a Cloudflare Tunnel necessarily produces a credential, and that
  # credential lands in state. Plan section 11.4 forbids plaintext secrets in
  # Terraform state, so state and plan files are encrypted at rest.
  #
  # It is deliberately NOT an `encryption` block referencing a variable: that
  # form cannot be evaluated statically, which breaks `tofu providers schema`
  # and any other command that runs before variables are resolved. See
  # infra/tofu/README.md for the exact TF_ENCRYPTION value to export.

  # Remote state on R2, configured with -backend-config at init time so that
  # bucket names and credentials stay out of the repository.
  # backend "s3" {}
}

provider "hcloud" {}

provider "cloudflare" {}
