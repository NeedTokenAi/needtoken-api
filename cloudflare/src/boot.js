export const PLACEHOLDER_DIGEST = "sha256:" + "0".repeat(64)
export const INACTIVITY_TIMEOUT_MS = 6 * 60 * 60 * 1000
export const ALARM_INTERVAL_MS = 60 * 1000
export const CONTAINER_PORT = 3000

// ISOLATED_SQL_HOSTS is the only SQL host this candidate may open.
// A test-fixture label is not a member of this list. Keep it aligned with
// cloudflareprofile/isolated_hosts.json.
export const ISOLATED_SQL_HOSTS = ["isolated.example"]

const PASSTHROUGH_ENV = [
  "CLOUDFLARE_PROFILE",
  "NEEDTOKEN_RUNTIME",
  "SQL_DSN",
  "SESSION_SECRET",
  "REDIS_CONN_STRING",
  "CRYPTO_SECRET",
  "NEEDTOKEN_LOG_SQL",
]

export function planBoot(env, images) {
  const source = env || {}
  if (source.CLOUDFLARE_PROFILE !== "1") {
    return refuse("profile off")
  }
  if (source.NEEDTOKEN_RUNTIME !== "isolated-candidate") {
    return refuse("production writer refused")
  }
  if (source.NEEDTOKEN_CONTAINER_BOOT !== "test-fixture") {
    return refuse("phase-1 skeleton does not boot a container")
  }
  const dsn = text(source.SQL_DSN)
  if (dsn === "") {
    return refuse("SQL_DSN binding is absent")
  }
  if (dsn.startsWith("local")) {
    return refuse("SQLite refused")
  }
  if (!isolatedSQLHost(dsn)) {
    return refuse("SQL host is outside the isolated allowlist")
  }
  if (text(source.SESSION_SECRET) === "") {
    return refuse("SESSION_SECRET binding is absent")
  }
  if (source.SESSION_SECRET === "random_string") {
    return refuse("placeholder SESSION_SECRET refused")
  }
  if (text(source.CRYPTO_SECRET) === "") {
    return refuse("CRYPTO_SECRET binding is absent")
  }
  if (text(source.REDIS_CONN_STRING) === "") {
    return refuse("REDIS_CONN_STRING binding is absent")
  }
  const logSQL = logDatabase(source)
  if (!logSQL.ok) {
    return logSQL
  }
  const size = parseInstance(source)
  if (!size.ok) {
    return size
  }
  const image = images && images.newapi
  const digest = imageDigest(imageReference(image))
  if (!String(image || "").startsWith("registry.cloudflare.com/") || !/^sha256:[0-9a-f]{64}$/.test(digest)) {
    return refuse("image digest is not pinned")
  }
  if (digest === PLACEHOLDER_DIGEST) {
    return refuse("placeholder digest")
  }
  const previous = imageDigest(imageReference(images && images.previous))
  if (previous && previous === digest) {
    return refuse("previous digest is not a second boot target")
  }
  // A real digest still does not authorize boot. The candidate records it
  // and stays fail-closed until a later phase that this skeleton is not.
  return {
    ok: false,
    authorizesBoot: false,
    isolated: true,
    reason: "phase-1 skeleton does not boot a container",
    start: {
      image,
      enableInternet: true,
      instance: size.instance,
      env: passThrough(source, logSQL),
    },
  }
}

export function isolatedSQLHost(dsn) {
  const host = sqlHost(dsn)
  return host !== "" && ISOLATED_SQL_HOSTS.includes(host)
}

export function sqlHost(dsn) {
  try {
    return new URL(String(dsn)).hostname.toLowerCase()
  } catch {
    return ""
  }
}

function logDatabase(source) {
  const mode = text(source.NEEDTOKEN_LOG_SQL)
  const dsn = text(source.LOG_SQL_DSN)
  if (mode === "main") {
    if (dsn !== "") {
      return refuse("LOG_SQL_DSN is set while logs stay on the main database")
    }
    return { ok: true, mode }
  }
  if (mode === "separate") {
    if (dsn === "") {
      return refuse("LOG_SQL_DSN binding is absent")
    }
    if (!isolatedSQLHost(dsn)) {
      return refuse("log SQL host is outside the isolated allowlist")
    }
    return { ok: true, mode }
  }
  return refuse("log database mode is not explicit")
}

export function parseInstance(env) {
  if (!text(env.CONTAINER_VCPU) || !text(env.CONTAINER_MEMORY_MIB) || !text(env.CONTAINER_DISK_MB)) {
    return refuse("container instance size is absent")
  }
  if (["basic", "lite", "dev", "standard"].includes(String(env.CONTAINER_INSTANCE || ""))) {
    return refuse("named instance sizes are not selected by this skeleton")
  }
  const vcpu = Number(env.CONTAINER_VCPU)
  const memoryMib = Number(env.CONTAINER_MEMORY_MIB)
  const diskMb = Number(env.CONTAINER_DISK_MB)
  const steps = Math.round(vcpu / 0.0625)
  if (!Number.isFinite(vcpu) || vcpu < 0.0625 || vcpu > 4 || Math.abs(steps * 0.0625 - vcpu) > 1e-9) {
    return refuse("container vcpu is outside the durable object limits")
  }
  if (!Number.isInteger(memoryMib) || memoryMib < 256 || memoryMib > 12288) {
    return refuse("container memory is outside the durable object limits")
  }
  if (!Number.isInteger(diskMb) || diskMb < 2000 || diskMb > 20000) {
    return refuse("container disk is outside the durable object limits")
  }
  return { ok: true, instance: { vcpu, memoryMib, diskMb } }
}

export function imageReference(value) {
  if (value && typeof value === "object") {
    return typeof value.image === "string" ? value.image : ""
  }
  return typeof value === "string" ? value : ""
}

export function imageDigest(image) {
  const text = String(image || "")
  const at = text.lastIndexOf("@")
  if (at < 0) {
    return ""
  }
  return text.slice(at + 1)
}

function passThrough(env, logSQL) {
  const out = { PORT: String(CONTAINER_PORT) }
  for (const key of PASSTHROUGH_ENV) {
    if (text(env[key]) !== "") {
      out[key] = env[key]
    }
  }
  if (logSQL && logSQL.mode === "separate" && text(env.LOG_SQL_DSN) !== "") {
    out.LOG_SQL_DSN = env.LOG_SQL_DSN
  }
  if (env.NEEDTOKEN_CONTAINER_BOOT === "test-fixture") {
    out.NEEDTOKEN_CONTAINER_BOOT = "test-fixture"
  }
  return out
}

function text(value) {
  return typeof value === "string" ? value.trim() : ""
}

function refuse(reason) {
  return { ok: false, reason }
}
