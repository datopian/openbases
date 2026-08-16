/**
 * Thin API client.
 *
 * Every response is treated as untrusted until it is validated. The UI never
 * decides authorisation: it renders what the API returns, and the API filters
 * at the domain and database layer. Hiding a control in the UI is not a
 * permission check.
 */

export interface VersionInfo {
  version: string;
  commit: string;
  built: string;
  env: string;
}

export interface Identity {
  subject: string;
  email: string;
  user_id: string;
  is_service: boolean;
}

export interface ProjectSummary {
  id: string;
  slug: string;
  name: string;
  portfolio: string;
  visibility: string;
  status: string;
  primary_owner: string;
  backup_owner: string;
  repositories: number;
}

export interface PullRequestView {
  number: number;
  title: string;
  state: string;
  author: string;
  checks_state: string;
  updated_at: string;
  merged_at: string | null;
}

export interface RepositoryStatus {
  full_name: string;
  pull_requests: PullRequestView[] | null;
  last_projected: string | null;
}

/** A derived statement, with the evidence behind it. */
export interface Signal {
  name: string;
  value: string;
  basis: string;
  observed_at: string | null;
}

export interface ProjectDetail extends ProjectSummary {
  repositories_detail?: RepositoryStatus[];
  repositories_list?: RepositoryStatus[];
  signals: Signal[];
}

export class ApiError extends Error {
  constructor(
    readonly status: number,
    message: string,
  ) {
    super(message);
    this.name = "ApiError";
  }
}

async function get<T>(path: string): Promise<T> {
  const res = await fetch(path, {
    headers: { Accept: "application/json" },
    credentials: "same-origin",
  });
  if (!res.ok) {
    throw new ApiError(res.status, `${path} returned ${res.status}`);
  }
  return (await res.json()) as T;
}

export const api = {
  version: () => get<VersionInfo>("/version"),
  me: () => get<Identity>("/v1/me"),
  projects: () => get<ProjectSummary[]>("/v1/projects"),
  projectDetail: (slug: string) =>
    get<ProjectDetail & { repositories: RepositoryStatus[] }>(
      `/v1/projects/${encodeURIComponent(slug)}/detail`,
    ),
};

/**
 * Human-readable age.
 *
 * Freshness is shown as an age rather than a timestamp because the question a
 * reader is actually asking is "is this still true", and "3 days ago" answers
 * it without arithmetic. The exact time stays in the title attribute for anyone
 * who needs it.
 */
export function age(iso: string | null): string {
  if (!iso) return "never";
  const then = new Date(iso).getTime();
  if (Number.isNaN(then)) return "unknown";
  const mins = Math.floor((Date.now() - then) / 60000);
  if (mins < 1) return "just now";
  if (mins < 60) return `${mins}m ago`;
  const hours = Math.floor(mins / 60);
  if (hours < 24) return `${hours}h ago`;
  const days = Math.floor(hours / 24);
  return `${days}d ago`;
}

/** True when an observation is old enough that a reader should doubt it. */
export function isStale(iso: string | null, hours = 24): boolean {
  if (!iso) return true;
  const then = new Date(iso).getTime();
  return Number.isNaN(then) || Date.now() - then > hours * 3600_000;
}
