import {
  TERMINAL_STATUSES,
  type Account,
  type CreateTransferRequest,
  type Entry,
  type ErrorCode,
  type Transfer,
} from "./types.js";

export interface ClientOptions {
  /** e.g. "http://localhost:8080" */
  baseUrl: string;
  /** Override fetch (tests, or a runtime without global fetch). */
  fetch?: typeof fetch;
  /** Retries after the first attempt for network errors, 429 and 5xx. Default 4. */
  maxRetries?: number;
  /** First backoff delay; doubles each retry with jitter. Default 200ms. */
  retryBaseDelayMs?: number;
  /** Per-attempt timeout. Default 10s. */
  timeoutMs?: number;
}

export class ForgeRailError extends Error {
  constructor(
    readonly status: number,
    readonly code: ErrorCode | "network_error" | "timeout",
    message: string,
    /** from the Retry-After header, if the server sent one */
    readonly retryAfterMs?: number,
  ) {
    super(message);
    this.name = "ForgeRailError";
  }

  /** True for failures where retrying the same request might work. */
  get retryable(): boolean {
    return this.status === 0 || this.status === 429 || this.status >= 500;
  }
}

export interface CreateTransferResult {
  transfer: Transfer;
  /** true if the server had already seen this idempotency key */
  replayed: boolean;
}

interface RequestOptions {
  body?: unknown;
  headers?: Record<string, string>;
  /** only safe to retry POSTs that carry an idempotency key */
  retry: boolean;
}

export class ForgeRailClient {
  private readonly baseUrl: string;
  private readonly fetchImpl: typeof fetch;
  private readonly maxRetries: number;
  private readonly baseDelay: number;
  private readonly timeoutMs: number;

  constructor(opts: ClientOptions) {
    this.baseUrl = opts.baseUrl.replace(/\/+$/, "");
    this.fetchImpl = opts.fetch ?? globalThis.fetch.bind(globalThis);
    this.maxRetries = opts.maxRetries ?? 4;
    this.baseDelay = opts.retryBaseDelayMs ?? 200;
    this.timeoutMs = opts.timeoutMs ?? 10_000;
  }

  async createAccount(name = ""): Promise<Account> {
    // not idempotent, so no automatic retries
    const { body } = await this.request<Account>("POST", "/v1/accounts", { body: { name }, retry: false });
    return body;
  }

  async getAccount(id: string): Promise<Account> {
    const { body } = await this.request<Account>("GET", `/v1/accounts/${encodeURIComponent(id)}`, { retry: true });
    return body;
  }

  async listEntries(accountId: string, opts: { limit?: number } = {}): Promise<Entry[]> {
    const q = opts.limit ? `?limit=${opts.limit}` : "";
    const { body } = await this.request<{ entries: Entry[] }>(
      "GET",
      `/v1/accounts/${encodeURIComponent(accountId)}/entries${q}`,
      { retry: true },
    );
    return body.entries;
  }

  /**
   * Creates a transfer. Every retry reuses the same idempotency key, so a
   * request that timed out after the server committed it will not move money
   * twice. Pass your own key if you want to retry across process restarts.
   */
  async createTransfer(
    req: CreateTransferRequest,
    opts: { idempotencyKey?: string } = {},
  ): Promise<CreateTransferResult> {
    const key = opts.idempotencyKey ?? newIdempotencyKey();
    const { body, headers } = await this.request<Transfer>("POST", "/v1/transfers", {
      body: req,
      headers: { "Idempotency-Key": key },
      retry: true,
    });
    return { transfer: body, replayed: headers.get("Idempotent-Replayed") === "true" };
  }

  async getTransfer(id: string): Promise<Transfer> {
    const { body } = await this.request<Transfer>("GET", `/v1/transfers/${encodeURIComponent(id)}`, { retry: true });
    return body;
  }

  /** Polls until the transfer reaches posted/settled/failed/rejected. */
  async waitForTransfer(id: string, opts: { timeoutMs?: number; intervalMs?: number } = {}): Promise<Transfer> {
    const deadline = Date.now() + (opts.timeoutMs ?? 60_000);
    const interval = opts.intervalMs ?? 500;
    for (;;) {
      const t = await this.getTransfer(id);
      if (TERMINAL_STATUSES.has(t.status)) return t;
      if (Date.now() + interval > deadline) {
        throw new ForgeRailError(0, "timeout", `transfer ${id} still ${t.status} after waiting`);
      }
      await sleep(interval);
    }
  }

  private async request<T>(
    method: string,
    path: string,
    opts: RequestOptions,
  ): Promise<{ body: T; headers: Headers }> {
    const attempts = opts.retry ? this.maxRetries + 1 : 1;
    let lastErr: ForgeRailError | undefined;
    for (let attempt = 0; attempt < attempts; attempt++) {
      if (attempt > 0) {
        await sleep(this.backoff(attempt, lastErr));
      }
      try {
        return await this.once<T>(method, path, opts);
      } catch (err) {
        if (!(err instanceof ForgeRailError) || !err.retryable) throw err;
        lastErr = err;
      }
    }
    throw lastErr!;
  }

  private async once<T>(method: string, path: string, opts: RequestOptions): Promise<{ body: T; headers: Headers }> {
    const ctrl = new AbortController();
    const timer = setTimeout(() => ctrl.abort(), this.timeoutMs);
    let res: Response;
    try {
      res = await this.fetchImpl(this.baseUrl + path, {
        method,
        headers: { "Content-Type": "application/json", ...opts.headers },
        body: opts.body === undefined ? undefined : JSON.stringify(opts.body),
        signal: ctrl.signal,
      });
    } catch (err) {
      const msg = err instanceof Error ? err.message : String(err);
      throw new ForgeRailError(0, ctrl.signal.aborted ? "timeout" : "network_error", msg);
    } finally {
      clearTimeout(timer);
    }

    const text = await res.text();
    let json: unknown = undefined;
    try {
      json = text ? JSON.parse(text) : undefined;
    } catch {
      // fall through, handled below
    }
    if (!res.ok) {
      const e = (json as { error?: { code?: ErrorCode; message?: string } } | undefined)?.error;
      throw new ForgeRailError(
        res.status,
        e?.code ?? "internal",
        e?.message ?? (text || res.statusText),
        parseRetryAfter(res.headers.get("Retry-After")),
      );
    }
    return { body: json as T, headers: res.headers };
  }

  private backoff(attempt: number, err?: ForgeRailError): number {
    if (err?.retryAfterMs !== undefined) return err.retryAfterMs;
    const exp = this.baseDelay * 2 ** (attempt - 1);
    return exp / 2 + Math.random() * (exp / 2);
  }
}

export function newIdempotencyKey(): string {
  const c = globalThis.crypto;
  if (c?.randomUUID) return c.randomUUID();
  // very old runtimes: good enough for a dedupe key, not for secrets
  return `${Date.now().toString(16)}-${Math.random().toString(16).slice(2)}`;
}

function parseRetryAfter(h: string | null): number | undefined {
  if (!h) return undefined;
  const secs = Number(h);
  return Number.isFinite(secs) && secs >= 0 ? Math.min(secs * 1000, 30_000) : undefined;
}

function sleep(ms: number): Promise<void> {
  return new Promise((r) => setTimeout(r, ms));
}
