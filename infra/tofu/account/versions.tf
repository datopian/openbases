terraform {
  required_version = ">= 1.9.0"

  # Configured by backend.hcl at init time.
  backend "s3" {}

  required_providers {
    cloudflare = {
      source  = "cloudflare/cloudflare"
      version = "~> 5.0"
    }
  }
}

provider "cloudflare" {}
