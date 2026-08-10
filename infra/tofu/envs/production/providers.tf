# Production root module — provider and backend configuration.
#
# Credentials are supplied by the environment (HCLOUD_TOKEN, CLOUDFLARE_API_TOKEN,
# GOOGLE_CREDENTIALS) or by the deployment pipeline. They are never written here
# and never committed.

terraform {
  required_version = ">= 1.8.0"

  required_providers {
    hcloud = {
      source  = "hetznercloud/hcloud"
      version = "~> 1.49"
    }
    cloudflare = {
      source  = "cloudflare/cloudflare"
      version = "~> 4.40"
    }
    google = {
      source  = "hashicorp/google"
      version = "~> 6.12"
    }
  }

  # Remote state. The backend is configured by WP-B1 with an R2-backed S3
  # endpoint. State contains resource identifiers, never secret values.
  # backend "s3" {}
}

provider "hcloud" {}

provider "cloudflare" {}

provider "google" {
  project = var.google_project_id
  region  = var.google_region
}
