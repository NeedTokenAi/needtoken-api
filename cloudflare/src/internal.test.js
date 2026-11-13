import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { chmodSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import {
  OUTBOUND_BODY_LIMIT_BYTES,
  POOL_BINDING,
  armPoolIntercept,
  internalFailedResponse,
  internalUnavailableResponse,
  isSingleDnsHostname,
  registerPoolIntercepts,
  requirePoolHost,
  routeInternal,
  trustEntrypoint,
  trustScript,
} from "./internal.js";

const POOL_HOST = "pool.example";
const PROXY_HOST = "proxy.example";

function poolRequest(url, init) {
  return new Request(url, init);
}

function poolProps(binding = POOL_BINDING) {
  return { host: POOL_HOST, binding };
}

async function withNoGlobalFetch(run) {
  const original = globalThis.fetch;
  globalThis.fetch = () => {
    throw new Error("global fetch was called");
  };
  try {
    return await run();
  } finally {
    globalThis.fetch = original;
  }
}

function bindingEnv(fetch) {
  return { [POOL_BINDING]: { fetch } };
}

async function callRoute(url, props, env, fetchBinding, init) {
  return withNoGlobalFetch(() => routeInternal(
    poolRequest(url, init),
    env,
    props,
    fetchBinding,
  ));
}

test("a legal pool host is forwarded to INTERNAL_POOL with manual redirect", async () => {
  let seen;
  const response = await callRoute(
    "https://pool.example/v1/chat/completions?n=1",
    poolProps(),
    bindingEnv(async () => new Response("ok", { status: 201 })),
    async (binding, request, init) => {
      seen = { request, init };
      return binding.fetch(request, init);
    },
    {
      method: "POST",
      headers: {
        authorization: "Bearer channel-key",
        "x-api-key": "key-1",
        "x-goog-api-key": "goog-1",
        "content-type": "application/json",
      },
      body: '{"n":1}',
    },
  );
  assert.equal(response.status, 201);
  assert.equal(seen.request.url, "https://pool.example/v1/chat/completions?n=1");
  assert.deepEqual(seen.init, { redirect: "manual" });
  assert.equal(seen.request.headers.get("authorization"), "Bearer channel-key");
  assert.equal(seen.request.headers.get("x-api-key"), "key-1");
  assert.equal(seen.request.headers.get("x-goog-api-key"), "goog-1");
  assert.equal([...seen.request.headers.keys()].some((name) => /internal-trust/i.test(name)), false);
});

test("host, binding, scheme, and credential mismatches are 403 and do not fetch", async () => {
  const cases = [
    ["https://other.example/v1", poolProps()],
    ["http://pool.example/v1", poolProps()],
    ["https://pool.example:8443/v1", poolProps()],
    ["https://pool.example/v1", { host: POOL_HOST, binding: "GLUE" }],
    ["https://pool.example/v1", { host: POOL_HOST, binding: "NEEDTOKEN_API" }],
    ["https://pool.example/v1", { host: POOL_HOST }],
  ];
  for (const [url, props] of cases) {
    let called = 0;
    const response = await callRoute(
      url,
      props,
      bindingEnv(async () => new Response("nope")),
      async () => {
        called += 1;
        throw new Error("binding must not be called");
      },
    );
    assert.equal(response.status, 403, url);
    assert.equal(response.headers.get("cache-control"), "no-store");
    assert.equal(called, 0);
  }

  let called = 0;
  const credentials = await withNoGlobalFetch(() => routeInternal(
    { url: "https://user:secret@pool.example/v1" },
    bindingEnv(async () => new Response("nope")),
    poolProps(),
    async () => {
      called += 1;
      throw new Error("binding must not be called");
    },
  ));
  assert.equal(credentials.status, 403);
  assert.equal(called, 0);
});

test("explicit port 443 is the same host and keeps the path", async () => {
  let seenUrl;
  const response = await callRoute(
    "https://pool.example:443/v1/responses",
    poolProps(),
    bindingEnv(async () => new Response(null, { status: 204 })),
    async (binding, request, init) => {
      seenUrl = request.url;
      return binding.fetch(request, init);
    },
  );
  assert.equal(response.status, 204);
  assert.equal(seenUrl, "https://pool.example/v1/responses");
});

test("a missing binding or a thrown fetch is 502 and does not call global fetch", async () => {
  const missing = await callRoute(
    "https://pool.example/v1/messages",
    poolProps(),
    {},
    async () => {
      throw new Error("binding must not be called");
    },
  );
  assert.equal(missing.status, 502);
  assert.equal(await missing.text(), "internal channel failed");
  assert.equal(missing.headers.get("cache-control"), "no-store");

  const thrown = await callRoute(
    "https://pool.example/v1/messages",
    poolProps(),
    bindingEnv(async () => {
      throw new Error("binding down");
    }),
    (binding, request, init) => binding.fetch(request, init),
  );
  assert.equal(thrown.status, 502);
  assert.equal(await thrown.text(), "internal channel failed");
  assert.equal(internalFailedResponse().status, 502);
});

test("a 302 to another host is returned and not followed", async () => {
  const response = await callRoute(
    "https://pool.example/v1/chat/completions",
    poolProps(),
    bindingEnv(async () => new Response(null, {
      status: 302,
      headers: { location: "https://evil.example/collect" },
    })),
    (binding, request, init) => binding.fetch(request, init),
  );
  assert.equal(response.status, 302);
  assert.equal(response.headers.get("location"), "https://evil.example/collect");
});

test("413 from the binding is returned and global fetch stays unused", async () => {
  const response = await callRoute(
    "https://pool.example/v1/chat/completions",
    poolProps(),
    bindingEnv(async () => new Response("too large", { status: 413 })),
    (binding, request, init) => binding.fetch(request, init),
    { method: "POST", body: "x" },
  );
  assert.equal(response.status, 413);
  assert.equal(await response.text(), "too large");
});

test("a stream body is not read and an aborted signal stays aborted", async () => {
  let pulled = false;
  const stream = new ReadableStream({
    pull(controller) {
      pulled = true;
      controller.enqueue(new Uint8Array([1, 2, 3]));
      controller.close();
    },
  });
  let forwarded;
  await callRoute(
    "https://pool.example/v1/chat/completions",
    poolProps(),
    bindingEnv(async () => new Response("ok")),
    async (binding, request, init) => {
      forwarded = request;
      assert.equal(pulled, false);
      assert.equal(request.bodyUsed, false);
      const bytes = new Uint8Array(await request.arrayBuffer());
      assert.deepEqual(bytes, new Uint8Array([1, 2, 3]));
      return binding.fetch(request, init);
    },
    { method: "POST", body: stream, duplex: "half" },
  );
  assert.equal(forwarded.bodyUsed, true);

  const controller = new AbortController();
  const request = poolRequest("https://pool.example/v1/chat/completions", {
    method: "POST",
    body: "hello",
    signal: controller.signal,
  });
  controller.abort();
  let aborted;
  await withNoGlobalFetch(() => routeInternal(
    request,
    bindingEnv(async () => new Response("ok")),
    poolProps(),
    async (binding, forwarded, init) => {
      aborted = forwarded.signal.aborted;
      return binding.fetch(forwarded, init);
    },
  ));
  assert.equal(aborted, true);
});

test("event-stream responses and ignored routing headers pass through", async () => {
  let forwarded;
  const response = await callRoute(
    "https://pool.example/v1/chat/completions",
    poolProps(),
    bindingEnv(async () => new Response("data: hi\n\n", {
      headers: { "content-type": "text/event-stream" },
    })),
    async (binding, request, init) => {
      forwarded = request;
      return binding.fetch(request, init);
    },
    {
      method: "POST",
      headers: {
        "X-Internal-Target": "https://evil.example/collect",
        "Cloudflare-Workers-Version-Overrides": "preview",
        authorization: "Bearer keep",
      },
      body: "{}",
    },
  );
  assert.equal(response.headers.get("content-type"), "text/event-stream");
  assert.equal(await response.text(), "data: hi\n\n");
  assert.equal(forwarded.headers.get("cloudflare-workers-version-overrides"), null);
  assert.equal(forwarded.headers.get("x-internal-target"), "https://evil.example/collect");
  assert.equal(forwarded.headers.get("authorization"), "Bearer keep");
  assert.equal(forwarded.url.includes("evil.example"), false);
});

test("only a single DNS hostname may arm the pool intercept", () => {
  assert.equal(isSingleDnsHostname(POOL_HOST), true);
  assert.equal(requirePoolHost({ INTERNAL_POOL_HOST: POOL_HOST }), POOL_HOST);
  const illegal = [
    undefined,
    "",
    "https://pool.example",
    "pool.example:443",
    "pool.example/v1",
    "user@pool.example",
    "*.example",
    "pool.example.",
    "1.2.3.4",
    "127.0.0.1",
    "::1",
    "Pool.example",
    "pool.example ",
    "a..b.com",
    "-bad.example",
    PROXY_HOST + ":8080",
  ];
  for (const host of illegal) {
    assert.equal(isSingleDnsHostname(host), false, String(host));
    assert.throws(
      () => requirePoolHost({ INTERNAL_POOL_HOST: host }),
      (error) => error.name === "InternalChannelUnavailable",
    );
  }
  assert.throws(
    () => requirePoolHost({}),
    (error) => error.name === "InternalChannelUnavailable",
  );
  const unavailable = internalUnavailableResponse(new Error("internal channel unavailable"));
  assert.equal(unavailable, undefined);
  const response = internalUnavailableResponse({ name: "InternalChannelUnavailable" });
  assert.equal(response.status, 503);
  assert.equal(response.headers.get("cache-control"), "no-store");
});

test("a proxy host is not registered and a bad host or intercept error does not start", async () => {
  const calls = [];
  const exportsApi = {
    InternalDeny(options) {
      return { kind: "deny", options };
    },
    InternalHop(options) {
      return { kind: "hop", options };
    },
  };
  const container = {
    running: true,
    async interceptOutboundHttp(host, handler) {
      calls.push(["http", host, handler]);
    },
    async interceptOutboundHttps(host, handler) {
      calls.push(["https", host, handler]);
    },
    start() {
      throw new Error("start must not run from armPoolIntercept");
    },
  };
  const armed = await armPoolIntercept(container, {
    INTERNAL_POOL_HOST: POOL_HOST,
    HTTPS_PROXY: `http://${PROXY_HOST}:8080`,
    PROXY_HOST,
  }, exportsApi);
  assert.deepEqual(calls.map((call) => `${call[0]} ${call[1]}`), [
    `http ${POOL_HOST}`,
    `https ${POOL_HOST}`,
  ]);
  assert.equal(JSON.stringify(calls).includes(PROXY_HOST), false);
  assert.equal(calls[1][2].options.props.binding, POOL_BINDING);
  assert.deepEqual(armed.entrypoint.slice(0, 2), ["sh", "-lc"]);
  assert.equal(calls.some((call) => call[1] === "*"), false);

  let intercepted = false;
  const idle = {
    running: false,
    async interceptOutboundHttp() {
      intercepted = true;
    },
    async interceptOutboundHttps() {
      intercepted = true;
    },
    start() {
      throw new Error("start must not run");
    },
  };
  await assert.rejects(
    () => armPoolIntercept(idle, { INTERNAL_POOL_HOST: `http://${PROXY_HOST}` }, exportsApi),
    (error) => error.name === "InternalChannelUnavailable",
  );
  assert.equal(intercepted, false);

  const failing = {
    running: false,
    async interceptOutboundHttp() {
      throw new Error("intercept failed");
    },
    async interceptOutboundHttps() {
      throw new Error("intercept failed");
    },
    start() {
      throw new Error("start must not run");
    },
  };
  await assert.rejects(
    () => armPoolIntercept(failing, { INTERNAL_POOL_HOST: POOL_HOST }, exportsApi),
    (error) => error.name === "InternalChannelUnavailable",
  );
});

test("the intercept list is the one pool host and the source has no star intercept", async () => {
  const calls = [];
  await registerPoolIntercepts({
    async interceptOutboundHttp(host) {
      calls.push(host);
    },
    async interceptOutboundHttps(host) {
      calls.push(host);
    },
  }, {
    InternalDeny() {
      return {};
    },
    InternalHop() {
      return {};
    },
  }, POOL_HOST);
  assert.deepEqual(calls, [POOL_HOST, POOL_HOST]);
  const internal = readFileSync(new URL("./internal.js", import.meta.url), "utf8");
  const index = readFileSync(new URL("./index.js", import.meta.url), "utf8");
  for (const source of [internal, index]) {
    assert.equal(source.includes('interceptOutboundHttps("*")'), false);
    assert.equal(source.includes("interceptOutboundHttps('*')"), false);
    assert.equal(source.includes("interceptAllOutboundHttp"), false);
  }
  assert.equal(internal.includes(".start("), false);
  const armAt = index.indexOf("armPoolIntercept(");
  const startAt = index.indexOf("container.start(");
  assert.ok(armAt > 0 && startAt > armAt);
  assert.equal(index.includes("enableInternet: true"), true);
  assert.equal(index.includes("if (!container.running)"), true);
  assert.equal(index.includes("instance-1"), false);
  const publicFetch = index.slice(index.indexOf("export default"));
  assert.equal(publicFetch.includes("INTERNAL_"), false);
  assert.equal(publicFetch.includes("interceptOutbound"), false);
  assert.equal(publicFetch.includes("X-Internal-Target"), false);
});

test("the entrypoint concatenates the CA bundles and execs /new-api", () => {
  const script = trustScript();
  assert.deepEqual(trustEntrypoint(), ["sh", "-lc", script]);
  assert.match(script, /^set -eu/);
  assert.match(script, /src='\/etc\/ssl\/certs\/ca-certificates\.crt'/);
  assert.match(script, /src='\/etc\/ssl\/cert\.pem'/);
  assert.match(script, /test -r "\$src"/);
  assert.match(script, /test -r '\/etc\/cloudflare\/certs\/cloudflare-containers-ca\.crt'/);
  assert.match(script, /cat "\$src" '\/etc\/cloudflare\/certs\/cloudflare-containers-ca\.crt' > '\/tmp\/needtoken-ca\.pem'/);
  assert.match(script, /export SSL_CERT_FILE='\/tmp\/needtoken-ca\.pem'/);
  assert.match(script, /exec '\/new-api'$/);
  assert.equal(script.includes("unset SSL_CERT_DIR"), false);
  assert.equal(script.includes("update-ca-certificates"), false);
});

test("the entrypoint uses the readable bundle and refuses an unreadable intercept CA", () => {
  const dir = mkdtempSync(join(tmpdir(), "needtoken-ca-"));
  try {
    const primary = join(dir, "ca.crt");
    const fallback = join(dir, "cert.pem");
    const cloudflare = join(dir, "cf.crt");
    const bundle = join(dir, "out.pem");
    const marker = join(dir, "ran");
    const command = join(dir, "cmd");
    writeFileSync(command, `#!/bin/sh\nprintf '%s' "$SSL_CERT_FILE" > '${marker}'\n`);
    chmodSync(command, 0o755);
    writeFileSync(primary, "image-ca\n");
    writeFileSync(fallback, "fallback-ca\n");
    writeFileSync(cloudflare, "intercept-ca\n");
    const ok = spawnSync("sh", ["-lc", trustScript({
      primary,
      fallback,
      cloudflare,
      bundle,
      command,
    })], { encoding: "utf8" });
    assert.equal(ok.status, 0);
    assert.equal(readFileSync(marker, "utf8"), bundle);
    assert.equal(readFileSync(bundle, "utf8"), "image-ca\nintercept-ca\n");

    chmodSync(primary, 0o000);
    const fallbackBundle = join(dir, "fallback-out.pem");
    const fallbackMarker = join(dir, "fallback-ran");
    const fallbackCommand = join(dir, "fallback-cmd");
    writeFileSync(fallbackCommand, `#!/bin/sh\nprintf '%s' "$SSL_CERT_FILE" > '${fallbackMarker}'\n`);
    chmodSync(fallbackCommand, 0o755);
    const usedFallback = spawnSync("sh", ["-lc", trustScript({
      primary,
      fallback,
      cloudflare,
      bundle: fallbackBundle,
      command: fallbackCommand,
    })], { encoding: "utf8" });
    assert.equal(usedFallback.status, 0);
    assert.equal(readFileSync(fallbackBundle, "utf8"), "fallback-ca\nintercept-ca\n");

    const missingMarker = join(dir, "missing-ran");
    const missingCommand = join(dir, "missing-cmd");
    writeFileSync(missingCommand, `#!/bin/sh\ntouch '${missingMarker}'\n`);
    chmodSync(missingCommand, 0o755);
    const missing = spawnSync("sh", ["-lc", trustScript({
      primary: fallback,
      fallback,
      cloudflare: join(dir, "absent.crt"),
      bundle: join(dir, "unused.pem"),
      command: missingCommand,
    })], { encoding: "utf8" });
    assert.notEqual(missing.status, 0);
    assert.throws(() => readFileSync(missingMarker, "utf8"));
  } finally {
    chmodSync(join(dir, "ca.crt"), 0o644);
    rmSync(dir, { recursive: true, force: true });
  }
});

test("the published entrypoint fails closed when the intercept CA is not on this machine", () => {
  const result = spawnSync("sh", ["-lc", trustScript()], { encoding: "utf8" });
  assert.notEqual(result.status, 0);
});

test("the entrypoint can be run twice without production files", () => {
  const dir = mkdtempSync(join(tmpdir(), "needtoken-ca-twice-"));
  try {
    const primary = join(dir, "ca.crt");
    const fallback = join(dir, "cert.pem");
    const cloudflare = join(dir, "cf.crt");
    const bundle = join(dir, "out.pem");
    const command = join(dir, "cmd");
    writeFileSync(primary, "image-ca\n");
    writeFileSync(fallback, "fallback-ca\n");
    writeFileSync(cloudflare, "intercept-ca\n");
    writeFileSync(command, "#!/bin/sh\nprintf '%s' \"$SSL_CERT_FILE\"\n");
    chmodSync(command, 0o755);
    const script = trustScript({ primary, fallback, cloudflare, bundle, command });
    const first = spawnSync("sh", ["-lc", script], { encoding: "utf8" });
    const second = spawnSync("sh", ["-lc", script], { encoding: "utf8" });
    assert.equal(first.status, 0);
    assert.equal(second.status, 0);
    assert.equal(first.stdout, bundle);
    assert.equal(second.stdout, bundle);
    assert.equal(readFileSync(bundle, "utf8"), "image-ca\nintercept-ca\n");
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

test("wrangler binds only INTERNAL_POOL and does not store the host", () => {
  const text = readFileSync(new URL("../wrangler.jsonc", import.meta.url), "utf8");
  const json = JSON.parse(text.replace(/^\s*\/\/.*$/gm, ""));
  assert.deepEqual(json.services, [{ binding: "INTERNAL_POOL", service: "needtoken-pool" }]);
  assert.equal(json.vars, undefined);
  assert.equal(JSON.stringify(json).includes("INTERNAL_POOL_HOST"), false);
  assert.equal(JSON.stringify(json).includes(POOL_HOST), false);
  assert.equal(JSON.stringify(json).includes(PROXY_HOST), false);
  assert.equal(text.includes("needtoken.ai"), false);
  assert.equal(json.containers[0].class_name, "NeedtokenApiContainer");
  assert.equal(json.durable_objects.bindings[0].name, "NEEDTOKEN_API");
  const workflow = readFileSync(new URL("../../.github/workflows/ci.yml", import.meta.url), "utf8");
  assert.equal(
    workflow.includes("node --test cloudflare/src/forward.test.js cloudflare/src/internal.test.js"),
    true,
  );
});

test("the intercept limit is smaller than the relay body limit this process already accepts", () => {
  const init = readFileSync(new URL("../../common/init.go", import.meta.url), "utf8");
  const bodyDefault = Number(init.match(/GetEnvOrDefault\("MAX_REQUEST_BODY_MB", (\d+)\)/)[1]);
  const fileDefault = Number(init.match(/GetEnvOrDefault\("MAX_FILE_DOWNLOAD_MB", (\d+)\)/)[1]);
  const apiBodyBytes = bodyDefault * 1024 * 1024;
  const fileBytes = fileDefault * 1024 * 1024;
  assert.equal(bodyDefault, 128);
  assert.equal(fileDefault, 64);
  assert.equal(OUTBOUND_BODY_LIMIT_BYTES, 100 * 1024 * 1024);
  assert.equal(OUTBOUND_BODY_LIMIT_BYTES, 104857600);
  assert.ok(OUTBOUND_BODY_LIMIT_BYTES < apiBodyBytes);
  const wrangler = JSON.parse(readFileSync(new URL("../wrangler.jsonc", import.meta.url), "utf8").replace(/^\s*\/\/.*$/gm, ""));
  assert.equal(wrangler.limits, undefined);
  assert.equal(wrangler.containers[0].outbound, undefined);
  const internalSource = readFileSync(new URL("./internal.js", import.meta.url), "utf8");
  assert.equal(internalSource.includes("interceptOutboundHttps(\n    host,\n    exportsApi.InternalHop"), true);
  assert.equal(/interceptOutboundHttps\([^)]*limit/.test(internalSource), false);

  const endpoints = readFileSync(new URL("../../common/endpoint_type.go", import.meta.url), "utf8");
  const block = endpoints.slice(
    endpoints.indexOf("case constant.ChannelTypeSub2API"),
    endpoints.indexOf("case constant.ChannelTypeCodex"),
  );
  for (const name of [
    "EndpointTypeOpenAI",
    "EndpointTypeOpenAIResponse",
    "EndpointTypeOpenAIResponseCompact",
    "EndpointTypeAnthropic",
    "EndpointTypeGemini",
    "EndpointTypeOpenAIAlphaSearch",
  ]) {
    assert.equal(block.includes(name), true, name);
  }
  assert.equal(block.includes("Audio"), false);
  assert.equal(block.includes("Video"), false);

  const adaptor = readFileSync(new URL("../../relay/channel/newapi/adaptor.go", import.meta.url), "utf8");
  const audio = adaptor.slice(
    adaptor.indexOf("func (a *Adaptor) ConvertAudioRequest"),
    adaptor.indexOf("func (a *Adaptor) DoRequest"),
  );
  assert.match(audio, /endpoint not supported/);
  const sub2api = readFileSync(new URL("../../relay/channel/sub2api/adaptor.go", import.meta.url), "utf8");
  assert.equal(sub2api.includes("ConvertAudioRequest"), false);

  const channel = readFileSync(new URL("../../model/channel.go", import.meta.url), "utf8");
  assert.match(channel, /BaseURL\s+\*string\s+`json:"base_url"/);
  assert.match(channel, /Setting\s+\*string\s+`json:"setting"/);
  const settings = readFileSync(new URL("../../relaykit/dto/channel_settings.go", import.meta.url), "utf8");
  assert.match(settings, /Proxy\s+string\s+`json:"proxy"`/);
  assert.ok(fileBytes < OUTBOUND_BODY_LIMIT_BYTES);
});
