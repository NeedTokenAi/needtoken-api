import assert from "node:assert/strict"
import test from "node:test"
import { ALARM_INTERVAL_MS, INACTIVITY_TIMEOUT_MS, planBoot } from "../src/boot.js"
import { createController, schedulerWait, userTrafficReady, waitForReady } from "../src/controller.js"
import { containerRequest, forwardUpstream } from "../src/proxy.js"
import { routeSingleton, secretBindings, SINGLETON_NAME } from "../src/singleton.js"

const DIGEST = "sha256:" + "ab".repeat(32)
const PREVIOUS = "sha256:" + "cd".repeat(32)

globalThis.scheduler = {
  async wait() {},
}

function fixtureEnv(overrides = {}) {
  return {
    CLOUDFLARE_PROFILE: "1",
    NEEDTOKEN_RUNTIME: "isolated-candidate",
    NEEDTOKEN_CONTAINER_BOOT: "test-fixture",
    NEEDTOKEN_LOG_SQL: "main",
    SQL_DSN: "postgres://isolated.example/needtoken",
    SESSION_SECRET: "fixture-session",
    CRYPTO_SECRET: "fixture-crypto",
    REDIS_CONN_STRING: "redis://isolated.example:6379",
    CONTAINER_VCPU: "1",
    CONTAINER_MEMORY_MIB: "2048",
    CONTAINER_DISK_MB: "8000",
    CLOUDFLARE_API_TOKEN: "do-not-copy",
    ...overrides,
  }
}

function memoryStorage(initial = {}) {
  const values = new Map(Object.entries(initial))
  const alarms = []
  return {
    values,
    alarms,
    async get(key) {
      return values.get(key)
    },
    async put(key, value) {
      values.set(key, value)
    },
    async setAlarm(timestamp) {
      alarms.push(timestamp)
    },
  }
}

function fakeContainer() {
  const calls = { start: 0, signal: [], timeout: [], monitor: 0, destroy: 0 }
  const container = {
    running: false,
    calls,
    start(options) {
      calls.start += 1
      calls.options = options
      container.running = true
    },
    signal(number) {
      calls.signal.push(number)
    },
    async setInactivityTimeout(ms) {
      calls.timeout.push(ms)
    },
    monitor() {
      calls.monitor += 1
    },
    destroy() {
      calls.destroy += 1
    },
    getTcpPort() {
      return {
        async fetch() {
          throw new Error("probe should be injected")
        },
      }
    },
  }
  return container
}

test("singleton routing ignores user-supplied instance names", () => {
  const names = []
  const namespace = {
    getByName(name) {
      names.push(name)
      return { name }
    },
  }
  const requests = [
    new Request("https://api.example/v1/chat?instance=customer-a"),
    new Request("https://api.example/v1/chat", { headers: { "x-instance": "customer-b" } }),
    new Request("https://evil.example/other-name"),
  ]
  for (const request of requests) {
    assert.equal(routeSingleton(namespace, request).name, SINGLETON_NAME)
  }
  assert.deepEqual(names, [SINGLETON_NAME, SINGLETON_NAME, SINGLETON_NAME])
  assert.throws(() => routeSingleton(undefined, requests[0]), /singleton binding is absent/)
})

test("secret bindings are names only", () => {
  assert.deepEqual(
    secretBindings.map((item) => item.secret),
    secretBindings.map((item) => item.binding),
  )
  for (const item of secretBindings) {
    assert.equal(Object.hasOwn(item, "value"), false)
  }
  assert.equal(secretBindings.find((item) => item.secret === "CRYPTO_SECRET").required, true)
  assert.equal(secretBindings.find((item) => item.secret === "LOG_SQL_DSN").explicit, "main-or-separate")
})

test("boot stays refused without the test switch and does not copy unrelated secrets", () => {
  const images = { newapi: "registry.cloudflare.com/ACCOUNT_ID/needtoken-api@" + DIGEST }
  const refused = planBoot(fixtureEnv({ NEEDTOKEN_CONTAINER_BOOT: "" }), images)
  assert.equal(refused.ok, false)
  assert.equal(refused.reason, "phase-1 skeleton does not boot a container")
  assert.equal(JSON.stringify(refused).includes("fixture-session"), false)

  const sqlite = planBoot(fixtureEnv({ SQL_DSN: "local-supersecret-do-not-leak" }), images)
  assert.equal(sqlite.ok, false)
  assert.equal(sqlite.reason, "SQLite refused")
  assert.equal(sqlite.reason.includes("supersecret"), false)

  const missingSize = planBoot(fixtureEnv({ CONTAINER_VCPU: "" }), images)
  assert.equal(missingSize.reason, "container instance size is absent")

  const basic = planBoot(fixtureEnv({ CONTAINER_INSTANCE: "basic" }), images)
  assert.equal(basic.ok, false)

  const production = planBoot(fixtureEnv({ SQL_DSN: "postgres://user:supersecret@ep-prod.neon.tech/needtoken" }), images)
  assert.equal(production.ok, false)
  assert.equal(production.reason, "SQL host is outside the isolated allowlist")
  assert.equal(JSON.stringify(production).includes("supersecret"), false)
  assert.equal(production.start, undefined)

  const missingCrypto = planBoot(fixtureEnv({ CRYPTO_SECRET: "" }), images)
  assert.equal(missingCrypto.reason, "CRYPTO_SECRET binding is absent")

  const allowed = planBoot(fixtureEnv(), images)
  assert.equal(allowed.authorizesBoot, false)
  assert.equal(allowed.isolated, true)
  assert.equal(allowed.ok, false)
  assert.equal(allowed.start.enableInternet, true)
  assert.deepEqual(allowed.start.instance, { vcpu: 1, memoryMib: 2048, diskMb: 8000 })
  assert.equal(allowed.start.env.SQL_DSN, "postgres://isolated.example/needtoken")
  assert.equal(allowed.start.env.CRYPTO_SECRET, "fixture-crypto")
  assert.equal(allowed.start.env.NEEDTOKEN_CONTAINER_BOOT, "test-fixture")
  assert.equal(allowed.start.env.LOG_SQL_DSN, undefined)
  assert.equal(allowed.start.env.CLOUDFLARE_API_TOKEN, undefined)
  assert.equal(allowed.start.image.endsWith(DIGEST), true)
})

test("streaming proxy returns the upstream response without reading it", async () => {
  let pulls = 0
  let release
  const blocked = new Promise((resolve) => {
    release = resolve
  })
  const stream = new ReadableStream({
    pull(controller) {
      pulls += 1
      if (pulls === 1) {
        controller.enqueue(new TextEncoder().encode("data: one\n\n"))
        return
      }
      return blocked.then(() => controller.close())
    },
  })
  const upstream = new Response(stream, { headers: { "content-type": "text/event-stream" } })
  const forwarded = forwardUpstream(upstream)
  assert.equal(forwarded, upstream)
  assert.equal(forwarded.bodyUsed, false)
  const reader = forwarded.body.getReader()
  const first = await reader.read()
  assert.equal(new TextDecoder().decode(first.value), "data: one\n\n")
  assert.equal(pulls, 1)
  release()
  await reader.cancel()
})

test("readiness stays closed until background is running, including at zero user traffic", async () => {
  const storage = memoryStorage()
  const container = fakeContainer()
  let status = {
    ready: false,
    background_running: false,
    fence_held: true,
    accepting: true,
    user_requests: 0,
  }
  const controller = createController({
    env: fixtureEnv(),
    images: {
      newapi: "registry.cloudflare.com/ACCOUNT_ID/needtoken-api@" + DIGEST,
      previous: "registry.cloudflare.com/ACCOUNT_ID/needtoken-api@" + PREVIOUS,
    },
    storage,
    container,
    now: () => 1_000,
    probe: async () => status,
    unitTestStart: true,
  })
  await controller.hydrate()
  assert.equal(storage.alarms.length, 1)
  const early = await controller.onFetch(new Request("https://api.example/v1/chat"))
  assert.equal(early.status, 503)
  assert.equal(container.calls.start, 0)

  status = {
    ready: true,
    background_running: true,
    fence_held: true,
    accepting: true,
    user_requests: 0,
  }
  await controller.onAlarm()
  assert.equal(controller.state.userRequests, 0)
  assert.equal(controller.state.backgroundContinuedWithoutUserTraffic, true)
  assert.equal(controller.state.readyForUsers, true)
  assert.equal(container.calls.destroy, 0)
  assert.equal(container.calls.timeout.at(-1), INACTIVITY_TIMEOUT_MS)
  assert.equal(storage.alarms.at(-1), 1_000 + ALARM_INTERVAL_MS)
  assert.equal(storage.values.get("containerEpoch"), 1)
  assert.equal(userTrafficReady(status, false), true)

  const upstream = new Response(new ReadableStream(), { headers: { "content-type": "text/event-stream" } })
  container.getTcpPort = () => ({
    fetch(input) {
      upstream.requested = input
      return Promise.resolve(upstream)
    },
  })
  const response = await controller.onFetch(new Request("https://api.example/v1/chat/completions"))
  assert.equal(response, upstream)
  assert.equal(response.bodyUsed, false)
  const hidden = await controller.onFetch(new Request("https://api.example/internal/cloudflare/runtime"))
  assert.equal(hidden.status, 404)
  const setup = await controller.onFetch(new Request("https://api.example/api/setup", { method: "POST" }))
  assert.equal(setup.status, 403)
})

test("watchdog keeps alarming when there is no user traffic and the probe fails", async () => {
  const storage = memoryStorage()
  const container = fakeContainer()
  container.running = true
  let failed = true
  const controller = createController({
    env: { CLOUDFLARE_PROFILE: "0" },
    storage,
    container,
    now: () => 50,
    probe: async () => {
      if (failed) {
        throw new Error("status down")
      }
      return {
        ready: true,
        background_running: true,
        fence_held: true,
        accepting: true,
        user_requests: 0,
      }
    },
  })
  await controller.hydrate()
  assert.equal(container.calls.start, 0)
  assert.equal(container.calls.monitor, 1)
  await controller.onAlarm()
  assert.equal(controller.state.readyForUsers, false)
  assert.equal(container.calls.destroy, 0)
  assert.equal(storage.values.get("containerEpoch"), undefined)
  failed = false
  await controller.onAlarm()
  assert.equal(controller.state.backgroundContinuedWithoutUserTraffic, true)
  assert.equal(controller.state.readyForUsers, true)
  assert.equal(storage.alarms.length, 3)
})

test("restart reuses the running container and the same epoch", async () => {
  const storage = memoryStorage({ singleton: SINGLETON_NAME, containerEpoch: 4 })
  const container = fakeContainer()
  container.running = true
  const controller = createController({
    env: fixtureEnv(),
    images: { newapi: "registry.cloudflare.com/ACCOUNT_ID/needtoken-api@" + DIGEST },
    storage,
    container,
    now: () => 10,
    probe: async () => ({
      ready: true,
      background_running: true,
      fence_held: true,
      accepting: true,
      user_requests: 0,
    }),
  })
  await controller.hydrate()
  await controller.onAlarm()
  assert.equal(container.calls.start, 0)
  assert.equal(storage.values.get("containerEpoch"), 4)
  assert.equal(container.calls.timeout[0], INACTIVITY_TIMEOUT_MS)
  await assert.rejects(
    createController({
      env: fixtureEnv(),
      storage: memoryStorage({ singleton: "other-instance" }),
      container: fakeContainer(),
      now: () => 10,
    }).hydrate(),
    /refusing non-singleton durable object/,
  )
})

test("SIGTERM drain does not release a live idle writer or destroy the container", async () => {
  const storage = memoryStorage()
  const container = fakeContainer()
  container.running = true
  const controller = createController({
    env: fixtureEnv(),
    storage,
    container,
    now: () => 10,
  })
  const script = [
    { accepting: true, fence_held: true, user_requests: 1, background_running: true },
    { accepting: false, fence_held: true, user_requests: 0, background_running: true },
    { accepting: false, fence_held: false, user_requests: 0, background_running: false },
  ]
  const result = await controller.drain(async () => script.shift(), 5)
  assert.equal(result.ok, true)
  assert.equal(result.destroyed, false)
  assert.equal(result.sawIdleWriter, true)
  assert.deepEqual(container.calls.signal, [15])
  assert.equal(container.calls.destroy, 0)
  assert.equal(container.calls.start, 0)
  const refused = await controller.onFetch(new Request("https://api.example/v1/chat"))
  assert.equal(refused.status, 503)
})

test("drain timeout does not start a second writer", async () => {
  const container = fakeContainer()
  container.running = true
  const controller = createController({
    env: fixtureEnv(),
    storage: memoryStorage(),
    container,
    now: () => 10,
  })
  const result = await controller.drain(async () => ({
    accepting: false,
    fence_held: true,
    user_requests: 0,
    background_running: true,
  }), 2)
  assert.equal(result.ok, false)
  assert.equal(result.sawIdleWriter, true)
  assert.equal(container.calls.destroy, 0)
  assert.equal(container.calls.start, 0)
})

function readyStatus() {
  return {
    ready: true,
    background_running: true,
    fence_held: true,
    accepting: true,
    user_requests: 0,
  }
}

test("readiness retries ECONNREFUSED and then becomes ready", async () => {
  const waits = []
  let probes = 0
  const status = await waitForReady(
    async () => {
      probes += 1
      if (probes < 3) {
        const error = new Error("connect ECONNREFUSED 127.0.0.1:3000")
        error.code = "ECONNREFUSED"
        throw error
      }
      return readyStatus()
    },
    async (ms) => {
      waits.push(ms)
    },
    5,
  )
  assert.equal(probes, 3)
  assert.deepEqual(waits, [200, 200])
  assert.equal(status.user_requests, 0)
  assert.equal(status.background_running, true)
})

test("a closed port, a failed start, a non-zero exit, and a rebuilt durable object stay on the current image", async () => {
  const storage = memoryStorage()
  const container = fakeContainer()
  let probes = 0
  const images = {
    newapi: "registry.cloudflare.com/ACCOUNT_ID/needtoken-api@" + DIGEST,
    previous: "registry.cloudflare.com/ACCOUNT_ID/needtoken-api@" + PREVIOUS,
  }
  container.start = (options) => {
    container.calls.start += 1
    container.calls.options = options
    if (container.calls.start === 1) {
      throw new Error("start failed")
    }
    container.running = true
  }
  container.monitor = () => {
    container.calls.monitor += 1
    if (container.calls.monitor === 1) {
      return Promise.reject(new Error("exit 1"))
    }
    return new Promise(() => {})
  }
  const controller = createController({
    env: fixtureEnv(),
    images,
    storage,
    container,
    now: () => 5_000,
    unitTestStart: true,
    probe: async () => {
      probes += 1
      if (probes === 1) {
        const error = new Error("connect ECONNREFUSED")
        error.code = "ECONNREFUSED"
        throw error
      }
      return readyStatus()
    },
  })
  await controller.hydrate()
  assert.equal(storage.alarms.length, 1)
  await controller.onAlarm()
  assert.equal(container.calls.start, 1)
  assert.equal(controller.state.lastBootError, "start failed")
  assert.equal(container.running, false)
  assert.equal(storage.values.get("containerEpoch"), undefined)

  await controller.onAlarm()
  assert.equal(container.calls.start, 2)
  assert.equal(container.calls.options.image.endsWith(DIGEST), true)
  assert.equal(container.calls.options.image.endsWith(PREVIOUS), false)
  assert.equal(controller.state.recovery.rollback, false)
  assert.equal(controller.state.recovery.switchesImage, false)
  assert.equal(storage.values.get("recovery").rollback, false)
  assert.equal(controller.state.userRequests, 0)
  assert.equal(controller.state.backgroundContinuedWithoutUserTraffic, true)

  container.running = false
  await new Promise((resolve) => setTimeout(resolve, 0))
  assert.equal(controller.state.lastExit, "nonzero")
  const rebuilt = createController({
    env: fixtureEnv(),
    images,
    storage,
    container,
    now: () => 9_000,
    unitTestStart: true,
    probe: async () => readyStatus(),
  })
  await rebuilt.hydrate()
  await rebuilt.onAlarm()
  assert.equal(container.calls.start, 3)
  assert.equal(container.calls.options.image.endsWith(DIGEST), true)
  assert.equal(rebuilt.state.userRequests, 0)
  assert.equal(container.calls.signal.length, 0)
  assert.equal(rebuilt.state.recovery.rollback, false)
})

test("proxy keeps the original request signal, stream, and payment body", async () => {
  const raw = JSON.stringify({ id: "pay_1", amount: "10.00", sign: "abc" })
  const payment = new Request("https://pay.example/api/payment/callback?x=1", {
    method: "POST",
    headers: { "content-type": "application/json", host: "pay.example" },
    body: raw,
  })
  const forwardedPayment = containerRequest(payment)
  assert.equal(forwardedPayment.url, "http://container/api/payment/callback?x=1")
  assert.equal(forwardedPayment.headers.get("host"), null)
  assert.equal(await forwardedPayment.text(), raw)

  const source = new AbortController()
  const stream = new ReadableStream({
    start(controller) {
      controller.enqueue(new TextEncoder().encode("data: token\n\n"))
    },
  })
  const live = new Request("https://api.example/v1/chat/completions", {
    method: "POST",
    headers: { "content-type": "text/event-stream", upgrade: "websocket" },
    body: stream,
    duplex: "half",
    signal: source.signal,
  })
  const forwarded = containerRequest(live)
  assert.equal(new URL(forwarded.url).host, "container")
  assert.equal(forwarded.headers.get("upgrade"), "websocket")
  assert.equal(forwarded.bodyUsed, false)
  source.abort()
  assert.equal(forwarded.signal.aborted, true)
  const reader = forwarded.body.getReader()
  const chunk = await reader.read()
  assert.equal(new TextDecoder().decode(chunk.value), "data: token\n\n")
  await reader.cancel()
})

test("scheduler.wait is required for the readiness delay", async () => {
  const previous = globalThis.scheduler
  globalThis.scheduler = undefined
  await assert.rejects(async () => {
    await schedulerWait(200)
  }, /scheduler\.wait is required/)
  globalThis.scheduler = previous
})
