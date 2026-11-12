import { ALARM_INTERVAL_MS, CONTAINER_PORT, INACTIVITY_TIMEOUT_MS, planBoot } from "./boot.js"
import { SINGLETON_NAME } from "./singleton.js"
import { containerRequest, forwardUpstream } from "./proxy.js"

const STATUS_PATH = "/internal/cloudflare/runtime"

export function createController(options) {
  const env = options.env || {}
  const container = options.container
  const storage = options.storage
  const now = options.now || (() => Date.now())
  const probe = options.probe || (() => probeRuntime(container))
  const plan = planBoot(env, options.images || {})
  const state = {
    plan,
    readyForUsers: false,
    draining: false,
    alarms: 0,
    userRequests: 0,
    backgroundContinuedWithoutUserTraffic: false,
    lastBootRefusal: plan.ok ? "" : plan.reason,
    lastStatus: null,
    started: false,
  }

  return {
    state,
    async hydrate() {
      const name = await storage.get("singleton")
      if (name && name !== SINGLETON_NAME) {
        throw new Error("refusing non-singleton durable object")
      }
      await storage.put("singleton", SINGLETON_NAME)
      if (container.running) {
        state.started = true
        await container.setInactivityTimeout(INACTIVITY_TIMEOUT_MS)
        container.monitor()
      }
      await storage.setAlarm(now() + ALARM_INTERVAL_MS)
    },
    async onAlarm() {
      state.alarms += 1
      try {
        if (container.running) {
          await container.setInactivityTimeout(INACTIVITY_TIMEOUT_MS)
        }
        const status = await probe()
        state.lastStatus = status
        if (status && status.user_requests === 0 && status.background_running === true) {
          state.backgroundContinuedWithoutUserTraffic = true
        }
        state.readyForUsers = userTrafficReady(status, state.draining)
        if (!container.running) {
          await startContainer(container, storage, plan, state)
        }
      } catch {
        state.readyForUsers = false
      }
      await storage.setAlarm(now() + ALARM_INTERVAL_MS)
    },
    async onFetch(request) {
      const url = new URL(request.url)
      if (url.pathname === STATUS_PATH) {
        return new Response(null, { status: 404 })
      }
      if (request.method === "POST" && url.pathname === "/api/setup") {
        return new Response(null, { status: 403 })
      }
      if (!state.readyForUsers || state.draining) {
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
        if (status && status.accepting === false && status.fence_held === false) {
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

async function startContainer(container, storage, plan, state) {
  if (container.running || state.started) {
    return
  }
  if (!plan.ok) {
    state.lastBootRefusal = plan.reason
    return
  }
  const current = Number((await storage.get("containerEpoch")) || 0)
  container.start(plan.start)
  state.started = true
  await storage.put("containerEpoch", current + 1)
  await container.setInactivityTimeout(INACTIVITY_TIMEOUT_MS)
  container.monitor()
}

export async function probeRuntime(container) {
  const response = await container.getTcpPort(CONTAINER_PORT).fetch("http://container" + STATUS_PATH)
  if (!response.ok) {
    throw new Error("runtime status unavailable")
  }
  return response.json()
}
