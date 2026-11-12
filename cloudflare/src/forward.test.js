import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import {
  APP_PORT,
  INACTIVITY_TIMEOUT_MS,
  INSTANCE_NAME,
  containerEnv,
  containerRequest,
  residentStub,
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
});
