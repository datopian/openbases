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

/** A repository attached to a project, as the registry holds it. */
export interface Repository {
  owner: string;
  name: string;
  full_name: string;
  provider: string;
  default_branch: string;
}

/**
 * What happened to one repository in an attach request.
 *
 * Per repository rather than per request, because attaching six at once is the
 * point: one already held by another project must not discard the other five,
 * and the reader has to be told which one it was.
 */
export interface AttachResult {
  full_name: string;
  status: "attached" | "already_attached" | "taken";
  taken_by?: string;
}

/** What creating a project needs. The primary owner defaults to the caller. */
export interface NewProject {
  slug: string;
  name: string;
  backup_owner: string;
  portfolio?: string;
  objective?: string;
  visibility?: string;
  primary_owner?: string;
  cell?: string;
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

/**
 * One bead as the PROJECT page shows it.
 *
 * Narrower than WorkItem, which the global Work page uses: that one carries
 * queue state and spend, and this is the subset ProjectDetail returns. Kept
 * separate rather than made optional on WorkItem, so a field that is always
 * present on one page is not typed as maybe-absent on both.
 */
export interface ProjectWorkItem {
  bead: string;
  title: string;
  kind: string;
  status: string;
  cell?: string;
  last_seen: string | null;
  /** Beads that must finish first. Absent when nothing blocks this one. */
  blocked_by?: string[];
  /** Beads waiting on this one: what finishing it releases. */
  blocking?: string[];
}

export interface ProjectDetail extends ProjectSummary {
  repositories_detail?: RepositoryStatus[];
  repositories_list?: RepositoryStatus[];
  signals: Signal[];
  /**
   * The project's beads.
   *
   * Optional on the type, not because the API omits it, but because a browser
   * may still be holding a page served by an older binary. Treated through
   * asList everywhere, so absent and empty behave the same.
   */
  work?: ProjectWorkItem[] | null;
}

/** What the platform is doing (wg-7bh). */
export interface PlatformState {
  observed_at: string;
  cells: PlatformCell[] | null;
  queue: Record<string, number> | null;
  graphs: PlatformGraph[] | null;
  unattributed: PlatformUnattributed[] | null;
  alerts: PlatformAlert[] | null;
  projects: number;
  repositories: number;
  beads: number;
}

export interface PlatformCell {
  cell: string;
  trust_domain: string;
  max_concurrent_agents: number;
  cpu_quota_percent: number | null;
  memory_limit_mb: number | null;
  projects: number;
  agents_live: number;
  agents_ever: number;
  rigs: string[] | null;
  last_report: string | null;
  escalations_last_hour: number;
}

export interface PlatformGraph {
  name: string;
  scope: string;
  project: string | null;
  cell: string | null;
  beads: number;
  last_seen: string | null;
}

export interface PlatformUnattributed {
  cell: string;
  graph: string | null;
  projects_on_cell: number;
  beads: number;
}

export interface PlatformAlert {
  rule: string;
  count: number;
  newest: string | null;
}

/** What happened to one bead (wg-m07). */
export interface BeadDetail {
  bead: string;
  title: string | null;
  kind: string | null;
  status: string | null;
  project: string | null;
  cell: string | null;
  last_seen: string | null;
  /**
   * Derived, not stored: `done` only when the run succeeded AND the bead is
   * closed. `blocked` is a run that exited zero and left the bead open — an
   * agent that reported it could not do the work. Treating that as `done` is
   * how people stop trusting the status field.
   */
  outcome:
    | "done"
    // Ran, produced a change that is in a pull request, and left the bead
    // open. Distinct from `blocked`: the work exists and awaits review.
    | "landed"
    | "blocked"
    | "failed"
    | "queued"
    | "running"
    | "never_dispatched";
  run: {
    job: string;
    status: string;
    rig: string | null;
    /** The harness and model, reported by the node when the run starts. Null
     * for runs that predate the columns, and for a job that never started. */
    harness: string | null;
    model: string | null;
    created: string | null;
    claimed: string | null;
    finished: string | null;
    log_tail: string | null;
  } | null;
  comment: { text: string; at: string | null; by: string | null } | null;
  /**
   * Where the work went. Empty is the honest answer for a bead whose work
   * needed no code change, and it was the only answer possible for anything
   * before the landing path existed: an agent cannot run git, so a change it
   * made stayed in a working tree on the execution node.
   *
   * Newest first. More than one means the bead was worked more than once.
   */
  pull_requests: {
    url: string;
    number: number;
    repository: string;
    head: string;
    base: string;
    opened: string | null;
  }[];
  spend: {
    /** True when this run's usage has not been imported yet. Cost arrives from
     * the gateway on an hourly timer, so a zero here means "not counted", not
     * "free" — and those are different answers to "what did that cost". */
    awaiting_import?: boolean;
    cents: string | number;
    calls: number;
    by_model:
      | {
          model: string;
          provider: string | null;
          calls: number;
          input_tokens: number;
          output_tokens: number;
          cents: string | number;
        }[]
      | null;
    /** Usage arrives on an hourly import, so this can lag the run. */
    newest_record: string | null;
  };
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

export interface WorkItem {
  bead: string;
  title: string;
  kind: string;
  status: string;
  cell: string;
  project?: string;
  last_seen: string;
  queue_state: string;
  queued_at: string;
  /** Decimal string, never a number. numeric(16,8) does not survive a float. */
  spent_cents: string;
  requests: number;
}

export interface QueueJob {
  id: string;
  kind: string;
  cell: string;
  rig: string;
  bead?: string;
  brief?: string;
  status: string;
  created_at: string;
  finished_at?: string;
  result?: string;
  project?: string;
}

/** POST that turns a refusal body into a readable error. */
async function post<T>(path: string, body: unknown): Promise<T> {
  const res = await fetch(path, {
    method: "POST",
    headers: { "Content-Type": "application/json", Accept: "application/json" },
    credentials: "same-origin",
    body: JSON.stringify(body),
  });
  const parsed: unknown = await res.json().catch(() => ({}));
  if (!res.ok) {
    const msg =
      parsed && typeof parsed === "object" && "error" in parsed
        ? String((parsed as { error: unknown }).error)
        : `HTTP ${res.status}`;
    throw new Error(msg);
  }
  return parsed as T;
}

async function del<T>(path: string): Promise<T> {
  const res = await fetch(path, {
    method: "DELETE",
    headers: { Accept: "application/json" },
    credentials: "same-origin",
  });
  const parsed: unknown = await res.json().catch(() => ({}));
  if (!res.ok) {
    const msg =
      parsed && typeof parsed === "object" && "error" in parsed
        ? String((parsed as { error: unknown }).error)
        : `HTTP ${res.status}`;
    throw new Error(msg);
  }
  return parsed as T;
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
    asList(
      (await get<{ branches: Branch[] | null }>("/v1/inbox/branches"))
        ?.branches,
    ),

  work: async () =>
    asList((await get<{ work: WorkItem[] | null }>("/v1/work"))?.work),
  workQueue: async () =>
    asList((await get<{ queue: QueueJob[] | null }>("/v1/work/queue"))?.queue),

  // The project is optional and, when given, is verified against the caller's
  // membership by system_enqueue_work. Sending "" rather than omitting it
  // would be a request to file into a project called empty string, so it is
  // left out instead.
  plan: async (brief: string, project?: string) =>
    post<{ job: string }>(
      "/v1/work/plan",
      project ? { brief, project } : { brief },
    ),

  // A budget refusal arrives as 402 with the reason in the body. Surfaced as
  // the error message rather than swallowed into "request failed", because the
  // reason is the single most useful thing this call can return.
  dispatch: async (bead: string) =>
    post<{ job: string; bead: string }>(
      `/v1/work/${encodeURIComponent(bead)}/dispatch`,
      {},
    ),

  decide: async (
    id: string,
    approve: boolean,
    reason: string,
    seenDigest: string,
  ) => {
    const res = await fetch(`/v1/approvals/${encodeURIComponent(id)}/decide`, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        Accept: "application/json",
      },
      credentials: "same-origin",
      body: JSON.stringify({ approve, reason, seen_digest: seenDigest }),
    });
    const body: unknown = await res.json().catch(() => ({}));
    if (!res.ok) {
      // A refusal is data, not a failure to display. It names the policy and
      // what to do, and the interface must show that rather than "forbidden".
      throw Object.assign(new ApiError(res.status, "refused"), {
        refusal: body as Refusal,
      });
    }
    return body;
  },

  // The unwrapped response, so a caller can report what actually arrived.
  // The device authorization flow's approval page (wg-8la).
  deviceRequest: (code: string) =>
    get<{
      client_label: string;
      scopes: string[];
      expires_at: string;
      approved: boolean;
    }>(`/v1/device/request?code=${encodeURIComponent(code)}`),
  deviceApprove: (code: string) =>
    post<{ approved: boolean }>("/v1/device/approve", { user_code: code }),

  projectsRaw: () => get<unknown>("/v1/projects"),

  questions: async () =>
    asList((await get<{ questions: string[] | null }>("/v1/ask"))?.questions),
  ask: (q: string) => get<Answer>(`/v1/ask?q=${encodeURIComponent(q)}`),

  projects: async () =>
    asList(await get<ProjectSummary[] | null>("/v1/projects")),
  platform: () => get<PlatformState>("/v1/platform"),

  bead: (id: string) => get<BeadDetail>(`/v1/work/${encodeURIComponent(id)}`),

  projectDetail: (slug: string) =>
    get<ProjectDetail & { repositories: RepositoryStatus[] }>(
      `/v1/projects/${encodeURIComponent(slug)}/detail`,
    ),

  createProject: async (n: NewProject) =>
    (await post<{ project: ProjectSummary }>("/v1/projects", n)).project,

  repositories: async (slug: string) =>
    asList(
      (
        await get<{ repositories: Repository[] | null }>(
          `/v1/projects/${encodeURIComponent(slug)}/repositories`,
        )
      )?.repositories,
    ),

  attachRepositories: async (slug: string, repositories: string[]) =>
    asList(
      (
        await post<{ results: AttachResult[] | null }>(
          `/v1/projects/${encodeURIComponent(slug)}/repositories`,
          { repositories },
        )
      )?.results,
    ),

  detachRepository: (slug: string, owner: string, name: string) =>
    del<{ status: string }>(
      `/v1/projects/${encodeURIComponent(slug)}/repositories/` +
        `${encodeURIComponent(owner)}/${encodeURIComponent(name)}`,
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
