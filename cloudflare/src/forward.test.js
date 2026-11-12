import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { test } from "node:test";
import { APP_PORT, containerEnv, containerRequest, waitForPort } from "./forward.js";

test("forwards to port 8080 and copies env strings unchanged", () => {
  assert.equal(APP_PORT, 8080);
  const sql = "postgres://user:secret@ep-prod.neon.tech/needtoken";
  const env = containerEnv({
    SQL_DSN: sql,
    SESSION_SECRET: "existing-session",
    CRYPTO_SECRET: "existing-crypto",
    REDIS_CONN_STRING: "rediss://existing.upstash.io",
    REDIS_KEY_PREFIX: "",
    PORT: "3000",
    NEEDTOKEN_API: { getByName() {} },
  });
  assert.deepEqual(env, {
    SQL_DSN: sql,
    SESSION_SECRET: "existing-session",
    CRYPTO_SECRET: "existing-crypto",
    REDIS_CONN_STRING: "rediss://existing.upstash.io",
    REDIS_KEY_PREFIX: "",
    PORT: "3000",
  });
  assert.equal(Object.hasOwn(env, "OPS_WRITES_ENABLED"), false);
});

test("does not add a redis prefix or inspect the database host", () => {
  const env = containerEnv({ SQL_DSN: "postgres://db.internal/app" });
  assert.deepEqual(Object.keys(env), ["SQL_DSN"]);
  assert.equal(env.SQL_DSN, "postgres://db.internal/app");
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

test("wrangler config is a digest placeholder with the production size", () => {
  const text = readFileSync(new URL("../wrangler.jsonc", import.meta.url), "utf8");
  const json = JSON.parse(text.replace(/^\s*\/\/.*$/gm, ""));
  assert.equal(json.name, "needtoken-api");
  assert.equal(json.workers_dev, true);
  assert.equal(json.routes, undefined);
  assert.equal(json.route, undefined);
  assert.equal(text.includes("needtoken.ai"), false);
  assert.equal(text.includes("Dockerfile"), false);
  assert.equal(text.includes("-staging"), false);
  const container = json.containers[0];
  assert.equal(container.max_instances, 2);
  assert.equal(container.instance_type.vcpu, 1);
  assert.equal(container.instance_type.memory_mib, 512);
  assert.match(container.image, /^registry\.cloudflare\.com\/ACCOUNT_ID\/IMAGE@sha256:0{64}$/);
  assert.equal(container.image.includes("needtoken-api"), false);
});
