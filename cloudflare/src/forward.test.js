import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import {
  APP_PORT,
  ENV_FINGERPRINT_KEY,
  INACTIVITY_TIMEOUT_MS,
  INSTANCE_NAME,
  PROBE_ATTEMPTS,
  READY_DEADLINE_MS,
  RETRY_AFTER_SECONDS,
  awaitInstance,
  containerEnv,
  containerEnvFingerprint,
  containerNotReadyError,
  containerRequest,
  emitRedisShapeOnce,
  ensureContainerStarted,
  notReadyResponse,
  redisBindingShape,
  residentStub,
  startupFailureResponse,
  waitForPort,
} from "./forward.js";

test("forwards to port 8080 and copies env strings unchanged", () => {
  assert.equal(APP_PORT, 8080);
  const sql = "postgres://user:secret@ep-prod.neon.tech/needtoken";
  const env = containerEnv({
    SQL_DSN: sql,
    SESSION_SECRET: "existing-session",
    CRYPTO_SECRET: "existing-crypto",
    REDIS_CONN_STRING: "rediss://existing.upstash.io",
    REDIS_KEY_PREFIX: "",
    NEEDTOKEN_API: { getByName() {} },
  });
  assert.deepEqual(env, {
    SQL_DSN: sql,
    SESSION_SECRET: "existing-session",
    CRYPTO_SECRET: "existing-crypto",
    REDIS_CONN_STRING: "rediss://existing.upstash.io",
    REDIS_KEY_PREFIX: "",
    PORT: "8080",
  });
  assert.equal(Object.hasOwn(env, "OPS_WRITES_ENABLED"), false);
});

test("sets PORT to 8080 and does not add a redis prefix", () => {
  const env = containerEnv({ SQL_DSN: "postgres://db.internal/app", PORT: "3000" });
  assert.deepEqual(env, { SQL_DSN: "postgres://db.internal/app", PORT: "8080" });
});

test("container request keeps the body and drops host", () => {
  const request = new Request("https://needtoken-api.workers.dev/v1/chat/completions", {
    method: "POST",
    headers: { host: "needtoken-api.workers.dev", "content-type": "application/json" },
    body: '{"n":1}',
  });
  const forwarded = containerRequest(request);
  assert.equal(forwarded.url, "http://container/v1/chat/completions");
  assert.equal(forwarded.headers.get("host"), null);
  assert.equal(forwarded.headers.get("content-type"), "application/json");
  assert.equal(forwarded.method, "POST");
});

test("waitForPort accepts the first response and retries a closed port", async () => {
  let calls = 0;
  await waitForPort(
    async () => {
      calls += 1;
      if (calls < 2) {
        throw new Error("ECONNREFUSED");
      }
      return { body: { cancel() {} } };
    },
    async () => {},
    3,
  );
  assert.equal(calls, 2);
});

test("a closed port is a retryable 503 and not an application 500", async () => {
  await assert.rejects(
    () => waitForPort(async () => {
      throw new Error("container did not become ready on port 8080");
    }, async () => {}, PROBE_ATTEMPTS),
    (error) => error.name === "ContainerNotReadyError",
  );
  const response = notReadyResponse();
  assert.equal(response.status, 503);
  assert.equal(response.headers.get("retry-after"), String(RETRY_AFTER_SECONDS));
  assert.equal(await response.text(), "container did not become ready on port 8080");
  assert.equal(startupFailureResponse(containerNotReadyError()).status, 503);
  assert.equal(startupFailureResponse(new Error("application failed")), undefined);
  assert.equal(PROBE_ATTEMPTS, 1);
  assert.ok(READY_DEADLINE_MS >= 3 * 60 * 1000);
  assert.ok(READY_DEADLINE_MS < 15 * 60 * 1000);
});

test("wrangler config builds the production Dockerfile in APAC", () => {
  const text = readFileSync(new URL("../wrangler.jsonc", import.meta.url), "utf8");
  const json = JSON.parse(text.replace(/^\s*\/\/.*$/gm, ""));
  assert.equal(json.name, "needtoken-api");
  assert.equal(json.workers_dev, false);
  assert.deepEqual(json.triggers.crons, ["* * * * *"]);
  assert.equal(json.preview_urls, false);
  assert.equal(json.routes, undefined);
  assert.equal(json.route, undefined);
  assert.equal(json.vars, undefined);
  assert.equal(json.observability.enabled, true);
  assert.equal(text.includes("needtoken.ai"), false);
  assert.equal(text.includes("-staging"), false);
  const serialized = JSON.stringify(json);
  for (const key of ["SESSION_COOKIE_TRUSTED_URL", "NEWAPI_BASE_URL", "GLUE_PUBLIC_URL"]) {
    assert.equal(serialized.includes(key), false);
  }
  const container = json.containers[0];
  assert.equal(container.scheduling_policy, "default");
  assert.equal(container.max_instances, 2);
  assert.equal(container.instance_type, "basic");
  assert.equal(container.image, "../Dockerfile");
  assert.equal(container.image_build_context, "..");
  assert.equal(container.image_vars, undefined);
  assert.deepEqual(container.constraints.regions, ["APAC"]);
  assert.equal(container.rollout_step_percentage, 100);
  assert.equal(container.rollout_active_grace_period, 0);
  assert.equal(json.migrations, undefined);
});

test("only instance-0 is addressed and sleep is the 6 hour maximum", () => {
  const names = [];
  const stub = residentStub({
    getByName(name) {
      names.push(name);
      return { name };
    },
  });
  assert.equal(INSTANCE_NAME, "instance-0");
  assert.deepEqual(names, ["instance-0"]);
  assert.equal(stub.name, "instance-0");
  assert.equal(INACTIVITY_TIMEOUT_MS, 6 * 60 * 60 * 1000);
  const worker = readFileSync(new URL("./index.js", import.meta.url), "utf8");
  const forward = readFileSync(new URL("./forward.js", import.meta.url), "utf8");
  assert.equal(worker.includes("instance-1"), false);
  assert.equal(worker.includes("getByName"), false);
  assert.equal(forward.includes("instance-1"), false);
  assert.equal(forward.includes("getByName(\""), false);
  assert.equal((worker.match(/residentStub\(/g) || []).length, 2);
});

test("cron retries the same instance and does not retry an application 500", async () => {
  const names = [];
  let calls = 0;
  const ready = await awaitInstance(
    async () => {
      calls += 1;
      names.push(INSTANCE_NAME);
      return new Response(null, { status: calls < 3 ? 503 : 200 });
    },
    async () => {},
    60_000,
  );
  assert.equal(ready.status, 200);
  assert.deepEqual(names, ["instance-0", "instance-0", "instance-0"]);

  let now = 0;
  let failures = 0;
  const exhausted = await awaitInstance(
    async () => {
      failures += 1;
      return new Response(null, { status: 503 });
    },
    async (ms) => {
      now += ms;
    },
    10_000,
    () => now,
  );
  assert.equal(exhausted.status, 503);
  assert.equal(failures, 3);

  let applicationCalls = 0;
  const application = await awaitInstance(
    async () => {
      applicationCalls += 1;
      return new Response(null, { status: 500 });
    },
    async () => {
      throw new Error("should not wait");
    },
  );
  assert.equal(application.status, 500);
  assert.equal(applicationCalls, 1);
});

const CANARY_USER = "synthetic-canary-user";
const CANARY_PASSWORD = "synthetic-canary-password";
const EXPECTED_HOST = "noble-sole-112190.upstash.io";

function redisUrl({ scheme = "rediss", user = CANARY_USER, password = CANARY_PASSWORD, host = EXPECTED_HOST, port = "6379", path = "", query = "", hash = "" } = {}) {
  const userinfo = password === undefined ? user : `${user}:${password}`;
  const portText = port === undefined ? "" : `:${port}`;
  return `${scheme}://${userinfo}@${host}${portText}${path}${query}${hash}`;
}

function assertNoSecret(text) {
  assert.equal(text.includes(CANARY_PASSWORD), false);
  assert.equal(text.includes(CANARY_USER), false);
  assert.equal(text.includes("rediss://"), false);
  assert.equal(text.includes("redis://"), false);
  assert.equal(text.includes(EXPECTED_HOST), false);
  assert.equal(text.includes("%"), false);
}

test("redis binding shape accepts rediss and reports an omitted port as 6379", () => {
  const explicit = redisBindingShape(redisUrl());
  assert.deepEqual(explicit, {
    valid: true,
    schemeRediss: true,
    hostMatch: true,
    port6379: true,
    passwordPresent: true,
  });
  const omitted = redisBindingShape(redisUrl({ port: undefined }));
  assert.equal(omitted.valid, true);
  assert.equal(omitted.port6379, true);
  assert.equal(omitted.schemeRediss, true);
  const plain = redisBindingShape(redisUrl({ scheme: "redis" }));
  assert.equal(plain.valid, true);
  assert.equal(plain.schemeRediss, false);
  assert.equal(plain.hostMatch, true);
  assert.equal(plain.port6379, true);
  assert.equal(plain.passwordPresent, true);
});

test("redis binding shape rejects the wrong host, port, or a missing password", () => {
  const wrongHost = redisBindingShape(redisUrl({ host: "evil.example" }));
  assert.equal(wrongHost.valid, true);
  assert.equal(wrongHost.hostMatch, false);
  assert.equal(wrongHost.port6379, true);
  const suffix = redisBindingShape(redisUrl({ host: `${EXPECTED_HOST}.evil.example` }));
  assert.equal(suffix.hostMatch, false);
  const wrongPort = redisBindingShape(redisUrl({ port: "6380" }));
  assert.equal(wrongPort.valid, true);
  assert.equal(wrongPort.port6379, false);
  const noPassword = redisBindingShape(`rediss://${EXPECTED_HOST}:6379/0`);
  assert.deepEqual(noPassword, {
    valid: true,
    schemeRediss: true,
    hostMatch: true,
    port6379: true,
    passwordPresent: false,
  });
  const userOnly = redisBindingShape(`rediss://${CANARY_USER}@${EXPECTED_HOST}`);
  assert.equal(userOnly.passwordPresent, false);
  assert.equal(userOnly.port6379, true);
  const userinfoHost = redisBindingShape(`rediss://${EXPECTED_HOST}:6379@evil.example`);
  assert.equal(userinfoHost.valid, true);
  assert.equal(userinfoHost.hostMatch, false);
  assert.equal(JSON.stringify(userinfoHost).includes(EXPECTED_HOST), false);
});

test("redis binding shape keeps encoded credentials out of the result", () => {
  const encoded = redisBindingShape(
    `rediss://${encodeURIComponent(CANARY_USER)}:${encodeURIComponent(`${CANARY_PASSWORD}:extra`)}@${EXPECTED_HOST}:6379/0`,
  );
  assert.equal(encoded.valid, true);
  assert.equal(encoded.hostMatch, true);
  assert.equal(encoded.passwordPresent, true);
  assert.equal(JSON.stringify(encoded).includes(CANARY_PASSWORD), false);
  const encodedHost = redisBindingShape(
    `rediss://${CANARY_USER}:${CANARY_PASSWORD}@${encodeURIComponent(EXPECTED_HOST)}:6379`,
  );
  assert.equal(encodedHost.valid, true);
  assert.equal(encodedHost.hostMatch, true);
  assertNoSecret(JSON.stringify(encodedHost));
  const encodedSuffix = redisBindingShape(
    `rediss://${encodeURIComponent(`${EXPECTED_HOST}.evil.example`)}:6379`,
  );
  assert.equal(encodedSuffix.hostMatch, false);
  const encodedNull = redisBindingShape(`rediss://evil.example%00.${EXPECTED_HOST}:6379`);
  assert.equal(encodedNull.hostMatch, false);
  assert.equal(JSON.stringify(encodedNull).includes("%00"), false);
});

test("malicious redis URLs collapse to valid false without an error string", () => {
  const samples = [
    "",
    "not a url",
    "http://noble-sole-112190.upstash.io:6379",
    `javascript:${CANARY_PASSWORD}`,
    `rediss://${EXPECTED_HOST}:6379#${CANARY_PASSWORD}`,
    `rediss://${EXPECTED_HOST}:6379/${CANARY_PASSWORD}`,
    `rediss://${EXPECTED_HOST}:6379\n${CANARY_PASSWORD}`,
    `rediss://${CANARY_USER}:${CANARY_PASSWORD}@${EXPECTED_HOST}:6379/${CANARY_PASSWORD}`,
    null,
    { href: `rediss://${CANARY_USER}:${CANARY_PASSWORD}@${EXPECTED_HOST}` },
  ];
  for (const sample of samples) {
    assert.deepEqual(redisBindingShape(sample), {
      valid: false,
      schemeRediss: false,
      hostMatch: false,
      port6379: false,
      passwordPresent: false,
    });
  }
});

test("a durable object lifetime logs the redis shape once and hides the canary", () => {
  const state = { logged: false };
  const lines = [];
  const raw = redisUrl({ query: `?note=${encodeURIComponent(CANARY_PASSWORD)}` });
  const first = emitRedisShapeOnce(state, (record) => lines.push(record), raw, {
    containerRunning: false,
    startWithInternet: true,
  });
  const second = emitRedisShapeOnce(
    state,
    (record) => lines.push(record),
    `redis://${CANARY_USER}:${CANARY_PASSWORD}@evil.example:6380`,
    { containerRunning: true, startWithInternet: false },
  );
  assert.equal(first, true);
  assert.equal(second, false);
  assert.equal(lines.length, 1);
  assert.deepEqual(lines[0], {
    event: "redis_binding_shape",
    source: "worker-binding",
    containerEnvChecked: false,
    valid: true,
    schemeRediss: true,
    hostMatch: true,
    port6379: true,
    passwordPresent: true,
    containerRunning: false,
    startWithInternet: true,
  });
  const rendered = JSON.stringify(lines[0]);
  assertNoSecret(rendered);
  assert.equal(rendered.includes(raw), false);
  const response = notReadyResponse();
  assert.equal(response.status, 503);
  assertNoSecret(response.headers.get("retry-after") + "container did not become ready on port 8080");
});

test("startup records the worker binding once and delegates env-change reload to ensureContainerStarted", () => {
  const worker = readFileSync(new URL("./index.js", import.meta.url), "utf8");
  assert.equal(worker.includes("emitRedisShapeOnce("), true);
  assert.equal(worker.includes("enableInternet: true"), true);
  assert.equal(worker.includes("ensureContainerStarted("), true);
  assert.equal(worker.includes("destroy("), false);
  assert.equal(worker.includes("console.log(JSON.stringify(record))"), true);
  assert.equal(worker.includes("console.log(this.env"), false);
  assert.equal((worker.match(/emitRedisShapeOnce\(/g) || []).length, 1);
});

test("containerEnvFingerprint is stable across key order and hashes without plaintext", async () => {
  const a = await containerEnvFingerprint({ NEEDTOKEN_LINKING_SECRET: CANARY_PASSWORD, SQL_DSN: "postgres://db/a", NEEDTOKEN_API: { getByName() {} } });
  const b = await containerEnvFingerprint({ SQL_DSN: "postgres://db/a", NEEDTOKEN_LINKING_SECRET: CANARY_PASSWORD });
  const c = await containerEnvFingerprint({ SQL_DSN: "postgres://db/a", NEEDTOKEN_LINKING_SECRET: "rotated-secret" });
  assert.equal(a, b);
  assert.notEqual(a, c);
  assert.match(a, /^[0-9a-f]{64}$/);
  assertNoSecret(a);
});

test("ensureContainerStarted destroys and restarts a running container only when env changes", async () => {
  const store = new Map();
  const storage = {
    async get(k) { return store.get(k); },
    async put(k, v) { store.set(k, v); },
  };
  const events = [];
  const container = {
    running: false,
    async destroy(reason) {
      events.push(["destroy", reason]);
      this.running = false;
    },
  };
  const startFn = (env) => {
    events.push(["start", env.NEEDTOKEN_LINKING_SECRET]);
    container.running = true;
  };

  const first = await ensureContainerStarted(container, storage, { NEEDTOKEN_LINKING_SECRET: "s1" }, startFn);
  assert.equal(first.started, true);
  assert.equal(first.restarted, false);
  assert.deepEqual(events, [["start", "s1"]]);
  assert.match(store.get(ENV_FINGERPRINT_KEY), /^[0-9a-f]{64}$/);
  assert.equal(store.get(ENV_FINGERPRINT_KEY).includes("s1"), false);

  // Same env after DO isolate reload: stays running without destroy.
  const steady = await ensureContainerStarted(container, storage, { NEEDTOKEN_LINKING_SECRET: "s1" }, startFn);
  assert.equal(steady.started, false);
  assert.equal(steady.restarted, false);
  assert.deepEqual(events, [["start", "s1"]]);

  // Updated secret while container is running: destroys once and restarts with new env.
  const rotated = await ensureContainerStarted(container, storage, { NEEDTOKEN_LINKING_SECRET: "s2" }, startFn, first.fingerprint);
  assert.equal(rotated.started, true);
  assert.equal(rotated.restarted, true);
  assert.equal(store.get(ENV_FINGERPRINT_KEY), rotated.fingerprint);
  assert.deepEqual(events, [
    ["start", "s1"],
    ["destroy", "env_changed"],
    ["start", "s2"],
  ]);
});
