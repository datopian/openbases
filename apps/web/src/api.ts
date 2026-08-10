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
};
