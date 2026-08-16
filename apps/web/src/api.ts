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

export interface AttentionItem {
  id: string;
  rule: string;
  score: number;
  explanation: Record<string, unknown>;
  status: string;
  project?: string;
  project_slug?: string;
  approval_request_id?: string;
  created_at: string;
  snoozed_until?: string | null;
}

export interface Branch {
  project_slug: string;
  project_name: string;
  candidates: AttentionItem[] | null;
  /** candidates | blocked | nothing_ready | none_visible */
  outcome: string;
  reason?: string;
  remaining: number;
}

export interface Refusal {
  code: string;
  policy: string;
  message: string;
  allowed_actor?: string;
  remediation?: string;
}

export interface Evidence {
  kind: string;
  ref: string;
  note?: string;
}

/** A single sentence with its provenance. */
export interface Claim {
  statement: string;
  /** observed | calculated | inferred */
  kind: string;
  basis?: string;
  evidence: Evidence[] | null;
  observed_at?: string | null;
}

export interface Answer {
  question: string;
  claims: Claim[] | null;
  scope: string;
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

/**
 * Coerce a value that should be a list into one.
 *
 * The API guarantees arrays, but the page must not depend on that being true
 * forever. A single `null` where a list was expected throws "x.map is not a
 * function", React unmounts the tree, and the reader gets a blank white page —
 * a total loss of interface caused by one empty collection. Degrading to an
 * empty list turns that into a page that renders and says there is nothing
 * here, which is both truthful and recoverable.
 */
export function asList<T>(value: T[] | null | undefined): T[] {
  return Array.isArray(value) ? value : [];
}

export const api = {
  version: () => get<VersionInfo>("/version"),
  me: () => get<Identity>("/v1/me"),
  // Normalised at the boundary. A null body would otherwise leave the caller
  // unable to distinguish "still loading" from "nothing here", and the page
  // spins on Loading forever — which is quieter than a crash and just as wrong.
  inbox: async () =>
    asList((await get<{ items: AttentionItem[] | null }>("/v1/inbox"))?.items),
  branches: async () =>
    asList((await get<{ branches: Branch[] | null }>("/v1/inbox/branches"))?.branches),

  decide: async (id: string, approve: boolean, reason: string, seenDigest: string) => {
    const res = await fetch(`/v1/approvals/${encodeURIComponent(id)}/decide`, {
      method: "POST",
      headers: { "Content-Type": "application/json", Accept: "application/json" },
      credentials: "same-origin",
      body: JSON.stringify({ approve, reason, seen_digest: seenDigest }),
    });
    const body: unknown = await res.json().catch(() => ({}));
    if (!res.ok) {
      // A refusal is data, not a failure to display. It names the policy and
      // what to do, and the interface must show that rather than "forbidden".
      throw Object.assign(new ApiError(res.status, "refused"), { refusal: body as Refusal });
    }
    return body;
  },

  // The unwrapped response, so a caller can report what actually arrived.
  projectsRaw: () => get<unknown>("/v1/projects"),

  questions: async () =>
    asList((await get<{ questions: string[] | null }>("/v1/ask"))?.questions),
  ask: (q: string) => get<Answer>(`/v1/ask?q=${encodeURIComponent(q)}`),

  projects: async () => asList(await get<ProjectSummary[] | null>("/v1/projects")),
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

