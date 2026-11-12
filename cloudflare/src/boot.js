import {
  PLACEHOLDER_DIGEST,
  attestedDigests,
  databaseHostAllowed,
  parseAllowlist,
  writerPermitAccepted,
} from "./gate.js"

export { PLACEHOLDER_DIGEST }
export const INACTIVITY_TIMEOUT_MS = 6 * 60 * 60 * 1000
export const ALARM_INTERVAL_MS = 60 * 1000
export const CONTAINER_PORT = 3000

const PASSTHROUGH_ENV = [
  "CLOUDFLARE_PROFILE",
  "NEEDTOKEN_RUNTIME",
  "SQL_DSN",
  "SESSION_SECRET",
  "REDIS_CONN_STRING",
  "CRYPTO_SECRET",
  "NEEDTOKEN_LOG_SQL",
  "DATABASE_HOST",
  "NEEDTOKEN_HOST_ALLOWLIST",
  "NEEDTOKEN_WRITER_PERMIT",
  "NEEDTOKEN_QUEUE_CONSUMERS",
]

export function planBoot(env, images) {
  const source = env || {}
  if (source.CLOUDFLARE_PROFILE !== "1") {
    return refuse("profile off")
  }
  if (source.NEEDTOKEN_RUNTIME !== "staging") {
    return refuse("production writer refused")
  }
  const dsn = text(source.SQL_DSN)
  if (dsn === "") {
    return refuse("SQL_DSN binding is absent")
  }
  if (dsn.startsWith("local")) {
    return refuse("SQLite refused")
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
  if (!writerPermitAccepted(source.NEEDTOKEN_WRITER_PERMIT)) {
    return refuse("writer permit is a test value")
  }
  if (text(source.NEEDTOKEN_QUEUE_CONSUMERS) !== "off") {
    return refuse("production queue consumers are not stopped")
  }
  const allowlist = parseAllowlist(source.NEEDTOKEN_HOST_ALLOWLIST)
  if (allowlist.length === 0) {
    return refuse("host allowlist is absent")
  }
  const databaseHost = sqlHost(dsn)
  if (databaseHost === "" || databaseHost !== text(source.DATABASE_HOST).toLowerCase() || !databaseHostAllowed(databaseHost, allowlist)) {
    return refuse("SQL host is outside the isolated allowlist")
  }
  const logSQL = logDatabase(source, allowlist, dsn)
  if (!logSQL.ok) {
    return logSQL
  }
  const size = parseInstance(source)
  if (!size.ok) {
    return size
  }
  const image = images && images.newapi
  const reference = imageReference(image)
  const attested = attestedDigests(source)
  if (attested.length !== 1 || attested[0] !== reference) {
    return refuse("image is not an attested build")
  }
  if (!reference.startsWith("registry.cloudflare.com/") || !reference.includes("@sha256:")) {
    return refuse("image digest is not pinned")
  }
  return {
    ok: true,
    authorizesBoot: true,
    isolated: true,
    rollback: false,
    attestedDigests: attested,
    start: {
      image,
      enableInternet: true,
      instance: size.instance,
      env: passThrough(source, logSQL),
    },
  }
}

export function sqlHost(dsn) {
  try {
    return new URL(String(dsn)).hostname.toLowerCase()
  } catch {
    return ""
  }
}

function logDatabase(source, allowlist, sqlDSN) {
  const mode = text(source.NEEDTOKEN_LOG_SQL)
  const dsn = text(source.LOG_SQL_DSN)
  if (mode === "main") {
    if (dsn === "") {
      return refuse("LOG_SQL_DSN binding is absent")
    }
    if (dsn !== sqlDSN) {
      return refuse("LOG_SQL_DSN is not the isolated main database")
    }
    return { ok: true, mode }
  }
  if (mode === "separate") {
    if (dsn === "") {
      return refuse("LOG_SQL_DSN binding is absent")
    }
    if (dsn === sqlDSN) {
      return refuse("LOG_SQL_DSN is not a separate isolated database")
    }
    const host = sqlHost(dsn)
    if (!databaseHostAllowed(host, allowlist)) {
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
  if (logSQL && logSQL.ok && text(env.LOG_SQL_DSN) !== "") {
    out.LOG_SQL_DSN = env.LOG_SQL_DSN
  }
  return out
}

function text(value) {
  return typeof value === "string" ? value.trim() : ""
}

function refuse(reason) {
  return { ok: false, authorizesBoot: false, rollback: false, attestedDigests: [], reason }
}
