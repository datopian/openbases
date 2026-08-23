variable "hcloud_token" {
  description = <<-EOT
    Token for the Hetzner project that holds ONLY the runners.

    Deliberately a separate project from the Workgraph environments. A token can
    delete every server in its project, this one is used by the least trusted
    thing we run, and the alternative puts the live control and execution nodes
    one compromised workflow away from deletion.
  EOT
  type        = string
  sensitive   = true
}

variable "cloudflare_account_id" {
  description = "Cloudflare account, for the management tunnel."
  type        = string
}

variable "cloudflare_zone_id" {
  description = "Zone the SSH hostname is created in."
  type        = string
}

variable "ssh_hostname" {
  description = <<-EOT
    Hostname for SSH over the tunnel, e.g. ssh-ci-runner.openbases.com.

    The runner needs no inbound port to do its job — it polls GitHub outbound —
    so this exists only for management. It is still an Access-protected tunnel
    rather than an open port, because a CI box holds a credential that can
    register runners for the whole organisation.
  EOT
  type        = string
}

variable "runner_server_type" {
  description = <<-EOT
    cx33 (4 vCPU / 8 GB) by default.

    Sized against measured usage rather than guessed: 143 job-hours in a month
    cost $35 on GitHub's hosted runners. One cx33 is EUR 8.49/month for 720 hours
    of availability, and at 4 vCPU it runs two GitHub-parity jobs at once
    (GitHub's standard Linux runner is 2 vCPU / 7 GB) — roughly 1,440 job-hours a
    month, ten times what was actually used.

    The point is not the saving. It is that the cost stops being a function of
    usage, so a busy month cannot exhaust a budget and block every repository.
  EOT
  type        = string
  default     = "cx33"
}

variable "image" {
  description = "Base image."
  type        = string
  default     = "ubuntu-24.04"
}

variable "location" {
  description = "Hetzner location."
  type        = string
  default     = "fsn1"
}

variable "admin_ssh_key_ids" {
  description = <<-EOT
    Hetzner SSH keys, for break-glass through the Hetzner console only. Inbound
    SSH from the internet stays closed; this is what lets somebody in when the
    tunnel itself is what is broken.
  EOT
  type        = list(string)
  default     = []
}

variable "cloudflared_version" {
  description = "Pinned connector version."
  type        = string
}

variable "cloudflared_sha256" {
  description = "Checksum of the pinned connector."
  type        = string
}

variable "enable_hetzner_backups" {
  description = <<-EOT
    Off by default, and that is the right default here.

    A runner holds no state worth recovering: it is rebuilt from this module and
    Ansible, and anything left on it between jobs is a bug rather than an asset.
    Paying 20% for images of a machine we would rather destroy than restore is
    money spent to make the wrong recovery convenient.
  EOT
  type        = bool
  default     = false
}
