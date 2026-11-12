import { ALARM_INTERVAL_MS, CONTAINER_PORT, INACTIVITY_TIMEOUT_MS, planBoot } from "./boot.js"
import { SINGLETON_NAME } from "./singleton.js"
import { containerRequest, forwardUpstream } from "./proxy.js"
import { recoveryState } from "./recovery.js"

const STATUS_PATH = "/internal/cloudflare/runtime"
const READINESS_ATTEMPTS = 20
const READINESS_WAIT_MS = 200

export function createController(options) {
  const env = options.env || {}
  const container = options.container
  const storage = options.storage
  const images = options.images || {}
  const now = options.now || (() => Date.now())
  const wait = options.wait || schedulerWait
  const probe = options.probe || (() => probeRuntime(container))
  const plan = planBoot(env, images)
  const state = {
    plan,
    images,
    readyForUsers: false,
    draining: false,
    alarms: 0,
    userRequests: 0,
    backgroundContinuedWithoutUserTraffic: false,
    lastBootRefusal: plan.authorizesBoot ? "" : plan.reason || "boot is not authorized",
    lastStatus: null,
    lastExit: null,
    alarmArmed: false,
    observing: false,
    recovery: recoveryState(images),
  }

  return {
    state,
    async hydrate() {
      const name = await storage.get("singleton")
      if (name && name !== SINGLETON_NAME) {
        throw new Error("refusing non-singleton durable object")
      }
      await storage.put("singleton", SINGLETON_NAME)
      await storage.put("recovery", state.recovery)
      if (container.running) {
        await container.setInactivityTimeout(INACTIVITY_TIMEOUT_MS)
        observeExit(container, storage, state, options.waitUntil)
      }
      await armAlarm(storage, state, now)
    },
    async onAlarm() {
      state.alarms += 1
      try {
        if (container.running) {
          await container.setInactivityTimeout(INACTIVITY_TIMEOUT_MS)
        } else {
          await startContainer(container, storage, plan, state, options)
        }
        if (!container.running) {
          state.readyForUsers = false
        } else {
          const status = await waitForReady(() => probe(), wait, READINESS_ATTEMPTS)
          state.lastStatus = status
          if (status && status.user_requests === 0 && status.background_running === true) {
            state.backgroundContinuedWithoutUserTraffic = true
          }
          state.readyForUsers = userTrafficReady(status, state.draining)
        }
      } catch {
        state.readyForUsers = false
      }
      await armAlarm(storage, state, now)
    },
    async onFetch(request) {
      const url = new URL(request.url)
      if (!state.alarmArmed) {
        await armAlarm(storage, state, now)
      }
      if (url.pathname === STATUS_PATH) {
        return new Response(null, { status: 404 })
      }
      if (request.method === "POST" && url.pathname === "/api/setup") {
        return new Response(null, { status: 403 })
      }
      if (!state.readyForUsers || state.draining || !container.running) {
        return new Response(null, { status: 503 })
      }
      state.userRequests += 1
      const upstream = await container.getTcpPort(CONTAINER_PORT).fetch(containerRequest(request))
      return forwardUpstream(upstream)
    },
    async drain(readStatus, maxSteps = 30) {
      state.draining = true
      state.readyForUsers = false
      container.signal(15)
      let sawIdleWriter = false
      for (let step = 0; step < maxSteps; step += 1) {
        const status = await readStatus()
        if (status && status.accepting === false && status.fence_held === false && status.background_running === false) {
          return { ok: true, destroyed: false, sawIdleWriter }
        }
        if (
          status &&
          status.accepting === false &&
          status.fence_held === true &&
          status.user_requests === 0 &&
          status.background_running === true
        ) {
          sawIdleWriter = true
        }
      }
      return { ok: false, destroyed: false, sawIdleWriter }
    },
  }
}

export function userTrafficReady(status, draining) {
  if (draining || !status) {
    return false
  }
  return status.ready === true && status.background_running === true && status.fence_held === true && status.accepting !== false
}

// waitForReady retries a closed port. Containers start() returns before the
// process listens, and getTcpPort().fetch throws until it does. Durable
// Object code waits with scheduler.wait, not setTimeout.
export async function waitForReady(probe, wait, attempts = READINESS_ATTEMPTS) {
  let lastError
  for (let attempt = 0; attempt < attempts; attempt += 1) {
    try {
      const status = await probe()
      if (userTrafficReady(status, false)) {
        return status
      }
      lastError = new Error("container port is not ready")
    } catch (error) {
      lastError = error
    }
    if (attempt + 1 < attempts) {
      await wait(READINESS_WAIT_MS)
    }
  }
  throw lastError instanceof Error ? lastError : new Error("container port is not ready")
}

export function schedulerWait(ms) {
  const scheduler = globalThis.scheduler
  if (!scheduler || typeof scheduler.wait !== "function") {
    throw new Error("Durable Object scheduler.wait is required")
  }
  return scheduler.wait(ms)
}

async function armAlarm(storage, state, now) {
  await storage.setAlarm(now() + ALARM_INTERVAL_MS)
  state.alarmArmed = true
}

async function startContainer(container, storage, plan, state, options) {
  if (container.running) {
    return
  }
  if (plan.authorizesBoot !== true || !plan.start) {
    state.lastBootRefusal = plan.reason || "boot is not authorized"
    return
  }
  try {
    container.start(plan.start)
  } catch {
    state.lastBootError = "start failed"
    return
  }
  const current = Number((await storage.get("containerEpoch")) || 0)
  await storage.put("containerEpoch", current + 1)
  state.recovery = recoveryState(state.images)
  await storage.put("recovery", state.recovery)
  await container.setInactivityTimeout(INACTIVITY_TIMEOUT_MS)
  observeExit(container, storage, state, options.waitUntil)
}

function observeExit(container, storage, state, waitUntil) {
  if (state.observing) {
    return
  }
  state.observing = true
  const pending = Promise.resolve()
    .then(() => container.monitor())
    .then(() => {
      state.lastExit = 0
    })
    .catch(() => {
      state.lastExit = "nonzero"
      state.recovery = recoveryState(state.images)
      state.recovery.lastExit = "nonzero"
      return storage.put("recovery", state.recovery)
    })
    .finally(() => {
      state.observing = false
    })
  if (typeof waitUntil === "function") {
    waitUntil(pending)
  }
}

export async function probeRuntime(container) {
  const response = await container.getTcpPort(CONTAINER_PORT).fetch("http://container" + STATUS_PATH)
  if (!response.ok) {
    throw new Error("runtime status unavailable")
  }
  return response.json()
}
