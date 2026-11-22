import assert from "node:assert/strict";
import test from "node:test";

import {
  ADMIN_ENTRY_PATH,
  CONSOLE_ENTRY_PATH,
  EDGE_ADMIN_COOKIE,
  EDGE_ADMIN_HEADER,
  handleEdgeAdminGate,
} from "./admin_gate.js";
import { containerEnv, containerRequest } from "./forward.js";

const TEST_SECRET = "n".repeat(48);

function createMockSql() {
  const tickets = new Map();
  const sessions = new Map();
  return {
    tickets,
    sessions,
    exec(query, ...params) {
      const q = query.replace(/\s+/g, " ").trim();
      if (q.startsWith("CREATE TABLE IF NOT EXISTS")) return { toArray: () => [] };
      if (q.startsWith("DELETE FROM nt_edge_admin_tickets WHERE expires_at <= ?")) {
        for (const [k, v] of tickets) if (v.expires_at <= params[0]) tickets.delete(k);
        return { toArray: () => [] };
      }
      if (q.startsWith("DELETE FROM nt_edge_admin_sessions WHERE expires_at <= ?")) {
        for (const [k, v] of sessions) if (v.expires_at <= params[0]) sessions.delete(k);
        return { toArray: () => [] };
      }
      if (q.startsWith("SELECT session_id, actor, reason, created_at, expires_at FROM nt_edge_admin_sessions")) {
        return { toArray: () => [...sessions.values()].filter((r) => r.expires_at > params[0]).sort((a, b) => b.created_at - a.created_at) };
      }
      if (q.startsWith("SELECT COUNT(*) AS cnt FROM nt_edge_admin_tickets")) {
        return { toArray: () => [{ cnt: [...tickets.values()].filter((r) => r.expires_at > params[0]).length }] };
      }
      if (q.startsWith("SELECT session_id FROM nt_edge_admin_sessions WHERE session_id = ?")) {
        const row = sessions.get(params[0]);
        return { toArray: () => (row && row.expires_at > params[1] ? [{ session_id: params[0] }] : []) };
      }
      if (q.startsWith("INSERT INTO nt_edge_admin_tickets")) {
        const [token_hash, actor, reason, ttl_seconds, created_at, expires_at] = params;
        tickets.set(token_hash, { token_hash, actor, reason, ttl_seconds, created_at, expires_at });
        return { toArray: () => [] };
      }
      if (q.startsWith("SELECT token_hash, actor, reason, ttl_seconds, expires_at FROM nt_edge_admin_tickets")) {
        const row = tickets.get(params[0]);
        return { toArray: () => (row && row.expires_at > params[1] ? [row] : []) };
      }
      if (q.startsWith("DELETE FROM nt_edge_admin_tickets WHERE token_hash = ?")) {
        tickets.delete(params[0]);
        return { toArray: () => [] };
      }
      if (q.startsWith("INSERT INTO nt_edge_admin_sessions")) {
        const [session_id, actor, reason, created_at, expires_at] = params;
        sessions.set(session_id, { session_id, actor, reason, created_at, expires_at });
        return { toArray: () => [] };
      }
      if (q === "DELETE FROM nt_edge_admin_sessions") {
        sessions.clear();
        return { toArray: () => [] };
      }
      if (q === "DELETE FROM nt_edge_admin_tickets") {
        tickets.clear();
        return { toArray: () => [] };
      }
      throw new Error(`unexpected SQL: ${q}`);
    },
  };
}

test("new-api preserves C-side endpoints while blocking admin UI and admin APIs with 404", async () => {
  const sql = createMockSql();
  const env = { EDGE_ADMIN_SECRET: TEST_SECRET };

  // Public C-side APIs, relay, and linking callbacks remain open; legacy UI pages require DISABLE_PUBLIC_CONSOLE="0".
  const publicRequests = [
    new Request("https://api.example/v1/chat/completions", { method: "POST" }),
    new Request("https://api.example/api/status"),
    new Request("https://api.example/api/pricing"),
    new Request("https://api.example/api/models"),
    new Request("https://api.example/api/user/self"),
    new Request("https://api.example/api/user/login", { method: "POST" }),
    new Request("https://api.example/api/user/needtoken-sso", { method: "POST" }),
    new Request("https://api.example/api/user/topup", { method: "POST" }),
    new Request("https://api.example/api/user/topup/self"),
    new Request("https://api.example/api/user/account-link/approve", { method: "POST" }),
    new Request("https://api.example/api/token/"),
    new Request("https://api.example/api/log/self"),
    new Request("https://api.example/api/log/token"),
    new Request("https://api.example/api/data/self"),
    new Request("https://api.example/api/data/flow/self"),
    new Request("https://api.example/api/audit/self"),
    new Request("https://api.example/api/task/self"),
    new Request("https://api.example/api/task/task_123/artifacts"),
    new Request("https://api.example/api/subscription/plans"),
    new Request("https://api.example/api/subscription/self"),
    new Request("https://api.example/oauth"),
    new Request("https://api.example/account-link"),
  ];
  for (const req of publicRequests) {
    const res = await handleEdgeAdminGate(req, env, sql);
    assert.equal(res.allow, true, `expected allow for ${req.method} ${req.url}`);
  }

  // Public admin UI & admin APIs are blocked with 404.
  const blockedRequests = [
    new Request("https://api.example/channels"),
    new Request("https://api.example/system-settings/billing"),
    new Request("https://api.example/users"),
    new Request("https://api.example/redemption-codes"),
    new Request("https://api.example/models"),
    new Request("https://api.example/setup"),
    new Request("https://api.example/usage-logs/audit"),
    new Request("https://api.example/api/option/"),
    new Request("https://api.example/api/option/", { method: "PUT" }),
    new Request("https://api.example/api/option/payment_compliance", { method: "POST" }),
    new Request("https://api.example/api/performance/stats"),
    new Request("https://api.example/api/models/"),
    new Request("https://api.example/api/models/search"),
    new Request("https://api.example/api/models", { method: "POST" }),
    new Request("https://api.example/api/log/"),
    new Request("https://api.example/api/data/"),
    new Request("https://api.example/api/data/users"),
    new Request("https://api.example/api/audit"),
    new Request("https://api.example/api/task"),
    new Request("https://api.example/api/user/topup"),
    new Request("https://api.example/api/user/"),
    new Request("https://api.example/api/user/search?keyword=a"),
    new Request("https://api.example/api/user/42"),
    new Request("https://api.example/api/channel/"),
    new Request("https://api.example/api/group/"),
    new Request("https://api.example/api/redemption/"),
    new Request("https://api.example/api/subscription/admin/plans"),
    new Request("https://api.example/api/subscription/admin/needtoken-member", { method: "POST" }),
    new Request("https://api.example/API/CHANNEL/"),
    new Request("https://api.example//api/channel/"),
    new Request("https://api.example/api/v1/../channel/"),
  ];
  for (const req of blockedRequests) {
    const res = await handleEdgeAdminGate(req, env, sql);
    assert.equal(res.response?.status, 404, `expected 404 for ${req.method} ${req.url}`);
  }
});

test("internal secret unlocks exact glue/ops admin APIs only and never pure admin UI/root APIs", async () => {
  const sql = createMockSql();
  const env = { EDGE_ADMIN_SECRET: TEST_SECRET };
  const headers = { [EDGE_ADMIN_HEADER]: TEST_SECRET };

  for (const allowedInternal of [
    new Request("https://api.example/api/subscription/admin/fulfillment", { headers }),
    new Request("https://api.example/api/subscription/admin/needtoken-member", { headers }),
    new Request("https://api.example/api/redemption/fulfillment", { headers }),
    new Request("https://api.example/api/channel/", { headers }),
    new Request("https://api.example/api/channel/test/7", { headers }),
    new Request("https://api.example/api/group/", { headers }),
    new Request("https://api.example/api/user/?p=0&page_size=50", { headers }),
    new Request("https://api.example/api/user/search?keyword=test", { headers }),
    new Request("https://api.example/api/option/", { method: "GET", headers }),
    new Request("https://api.example/api/option/", { method: "PUT", headers }),
    new Request("https://api.example/api/data/users?start_timestamp=1&end_timestamp=2", { method: "GET", headers }),
  ]) {
    const res = await handleEdgeAdminGate(allowedInternal, env, sql);
    assert.equal(res.allow, true, `expected internal allow for ${allowedInternal.method} ${allowedInternal.url}`);
  }

  // Even with internal secret, pure root/UI endpoints and option/data sub-paths stay closed unless a break-glass cookie is issued.
  for (const stillBlocked of [
    new Request("https://api.example/system-settings", { headers }),
    new Request("https://api.example/api/option/", { method: "POST", headers }),
    new Request("https://api.example/api/option/", { method: "DELETE", headers }),
    new Request("https://api.example/api/option/payment_compliance", { method: "POST", headers }),
    new Request("https://api.example/api/option/model_pricing", { method: "GET", headers }),
    new Request("https://api.example/api/data/", { method: "GET", headers }),
    new Request("https://api.example/api/data/users", { method: "POST", headers }),
    new Request("https://api.example/api/data/flow", { method: "GET", headers }),
    new Request("https://api.example/api/performance/stats", { headers }),
  ]) {
    const res = await handleEdgeAdminGate(stillBlocked, env, sql);
    assert.equal(res.response?.status, 404, `expected 404 for ${stillBlocked.method} ${stillBlocked.url}`);
  }

  // Break-glass ticket unlocks admin UI and root APIs for that browser only, and revokes cleanly.
  let now = 1_700_000_000_000;
  const issue = await handleEdgeAdminGate(
    new Request("https://api.example/__needtoken_edge/admin-gate/ticket", {
      method: "POST",
      headers: { "content-type": "application/json", ...headers },
      body: JSON.stringify({ actor: "owner-1", reason: "inspect option", ttl_minutes: 30 }),
    }),
    env,
    sql,
    () => now,
  );
  const { unlock_path: unlockPath } = await issue.response.json();
  const unlock = await handleEdgeAdminGate(new Request(`https://api.example${unlockPath}`), env, sql, () => now);
  assert.equal(unlock.response?.status, 302);
  assert.equal(unlock.response.headers.get("location"), ADMIN_ENTRY_PATH);
  const cookiePair = unlock.response.headers.get("set-cookie").split(";")[0];
  assert.equal(cookiePair.startsWith(`${EDGE_ADMIN_COOKIE}=`), true);

  const adminPage = await handleEdgeAdminGate(
    new Request("https://api.example/api/option/payment_compliance", { method: "POST", headers: { cookie: cookiePair } }),
    env,
    sql,
    () => now,
  );
  assert.equal(adminPage.allow, true);

  await handleEdgeAdminGate(
    new Request("https://api.example/__needtoken_edge/admin-gate/revoke", { method: "POST", headers }),
    env,
    sql,
    () => now,
  );
  const afterRevoke = await handleEdgeAdminGate(
    new Request("https://api.example/api/option/payment_compliance", { method: "POST", headers: { cookie: cookiePair } }),
    env,
    sql,
    () => now,
  );
  assert.equal(afterRevoke.response?.status, 404);
});

test("zero-downtime rollout allows whitelisted internal APIs only when secret is unset unless enforced", async () => {
  const sql = createMockSql();
  // Secret unset: pure admin UI, root APIs, and strict internal ops APIs are ALWAYS 404 (fail-closed).
  assert.equal((await handleEdgeAdminGate(new Request("https://api.example/system-settings"), {}, sql)).response?.status, 404);
  assert.equal((await handleEdgeAdminGate(new Request("https://api.example/api/option/"), {}, sql)).response?.status, 404);
  assert.equal((await handleEdgeAdminGate(new Request("https://api.example/api/option/", { method: "PUT" }), {}, sql)).response?.status, 404);
  assert.equal((await handleEdgeAdminGate(new Request("https://api.example/api/data/users"), {}, sql)).response?.status, 404);
  assert.equal((await handleEdgeAdminGate(new Request("https://api.example/api/channel/"), {}, sql)).allow, true);

  // When EDGE_ADMIN_ENFORCE_API=1 without secret, fail closed.
  assert.equal(
    (await handleEdgeAdminGate(new Request("https://api.example/api/channel/"), { EDGE_ADMIN_ENFORCE_API: "1" }, sql)).response?.status,
    404,
  );
});

test("edge secrets are filtered from container env and stripped before forwarding", () => {
  const env = containerEnv({
    SQL_DSN: "postgres://db/api",
    EDGE_ADMIN_SECRET: TEST_SECRET,
    DISABLE_PUBLIC_ADMIN: "1",
    REDIRECT_ROOT_TO_CONSOLE: "1",
    DISABLE_PUBLIC_CONSOLE: "1",
    STOREFRONT_CONSOLE_ORIGIN: "https://needtoken.ai",
  });
  assert.equal(Object.hasOwn(env, "EDGE_ADMIN_SECRET"), false);
  assert.equal(Object.hasOwn(env, "DISABLE_PUBLIC_ADMIN"), false);
  assert.equal(Object.hasOwn(env, "REDIRECT_ROOT_TO_CONSOLE"), false);
  assert.equal(Object.hasOwn(env, "DISABLE_PUBLIC_CONSOLE"), false);
  assert.equal(Object.hasOwn(env, "STOREFRONT_CONSOLE_ORIGIN"), false);

  const fwd = containerRequest(new Request("https://api.example/api/channel/", {
    headers: { [EDGE_ADMIN_HEADER]: TEST_SECRET, authorization: "Bearer nap_x" },
  }));
  assert.equal(fwd.headers.get(EDGE_ADMIN_HEADER), null);
  assert.equal(fwd.headers.get("authorization"), "Bearer nap_x");
});

test("public console web routes redirect 302 to storefront member center unless opted out or break-glass unlocked", async () => {
  const sql = createMockSql();
  const env = { EDGE_ADMIN_SECRET: TEST_SECRET };

  const redirectCases = [
    ["https://api.example/", "https://needtoken.ai/me"],
    ["https://api.example/dashboard", "https://needtoken.ai/me"],
    ["https://api.example/sign-in", "https://needtoken.ai/auth/login?redirect=%2Fme"],
    ["https://api.example/login", "https://needtoken.ai/auth/login?redirect=%2Fme"],
    ["https://api.example/keys", "https://needtoken.ai/me/keys"],
    ["https://api.example/logs", "https://needtoken.ai/me/usage"],
    ["https://api.example/usage", "https://needtoken.ai/me/usage"],
    ["https://api.example/topup", "https://needtoken.ai/me/assets"],
    ["https://api.example/subscription", "https://needtoken.ai/plans"],
    ["https://api.example/pricing", "https://needtoken.ai/models"],
  ];
  for (const [url, expectedLocation] of redirectCases) {
    const res = await handleEdgeAdminGate(new Request(url), env, sql);
    assert.equal(res.response?.status, 302, `expected 302 for ${url}`);
    assert.equal(res.response.headers.get("location"), expectedLocation);
    assert.equal(res.response.headers.get("cache-control"), "no-store");
  }

  // AI protocol, public API, internal console API, and static assets stay unaffected.
  for (const req of [
    new Request("https://api.example/v1/models"),
    new Request("https://api.example/v1/chat/completions", { method: "POST" }),
    new Request("https://api.example/api/status"),
    new Request("https://api.example/api/pricing"),
    new Request("https://api.example/assets/index.js"),
    new Request("https://api.example/api/subscription/admin/needtoken-console/users/7/tokens", {
      headers: { [EDGE_ADMIN_HEADER]: TEST_SECRET },
    }),
  ]) {
    const res = await handleEdgeAdminGate(req, env, sql);
    assert.equal(res.allow, true, `expected allow for ${req.method} ${req.url}`);
  }

  // Opt-out via DISABLE_PUBLIC_CONSOLE="0" restores legacy console behavior (root -> /dashboard, /dashboard allowed).
  const legacyRoot = await handleEdgeAdminGate(
    new Request("https://api.example/"),
    { ...env, DISABLE_PUBLIC_CONSOLE: "0" },
    sql,
  );
  assert.equal(legacyRoot.response?.status, 302);
  assert.equal(legacyRoot.response.headers.get("location"), CONSOLE_ENTRY_PATH);

  const legacyDash = await handleEdgeAdminGate(
    new Request("https://api.example/dashboard"),
    { ...env, DISABLE_PUBLIC_CONSOLE: "0" },
    sql,
  );
  assert.equal(legacyDash.allow, true);

  const legacyNoRootRedirect = await handleEdgeAdminGate(
    new Request("https://api.example/"),
    { ...env, DISABLE_PUBLIC_CONSOLE: "0", REDIRECT_ROOT_TO_CONSOLE: "0" },
    sql,
  );
  assert.equal(legacyNoRootRedirect.allow, true);

  // Valid break-glass cookie bypasses public console redirect for emergency admin access.
  const now = 1_700_000_000_000;
  const issue = await handleEdgeAdminGate(
    new Request("https://api.example/__needtoken_edge/admin-gate/ticket", {
      method: "POST",
      headers: { "content-type": "application/json", [EDGE_ADMIN_HEADER]: TEST_SECRET },
      body: JSON.stringify({ actor: "owner-1", reason: "inspect root", ttl_minutes: 30 }),
    }),
    env,
    sql,
    () => now,
  );
  const { unlock_path: unlockPath } = await issue.response.json();
  const unlock = await handleEdgeAdminGate(new Request(`https://api.example${unlockPath}`), env, sql, () => now);
  const cookiePair = unlock.response.headers.get("set-cookie").split(";")[0];

  for (const url of ["https://api.example/", "https://api.example/sign-in", "https://api.example/dashboard"]) {
    const breakGlass = await handleEdgeAdminGate(
      new Request(url, { headers: { cookie: cookiePair } }),
      env,
      sql,
      () => now,
    );
    assert.equal(breakGlass.allow, true, `expected break-glass allow for ${url}`);
  }
});
