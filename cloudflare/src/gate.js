export const PLACEHOLDER_DIGEST = "sha256:" + "0".repeat(64)
export const BUILD_REPOSITORY = "NeedTokenAi/needtoken-api"
export const STAGING_IMAGE_NAME = "needtoken-api-staging"

const TEST_PERMITS = new Set([
  "approved",
  "approve",
  "test",
  "test-fixture",
  "fixture",
  "true",
  "false",
  "1",
  "0",
  "yes",
  "no",
  "isolated",
  "isolated-candidate",
  "isolated.example",
  "placeholder",
  "random_string",
])

const FULL_IMAGE = /^registry\.cloudflare\.com\/[a-z0-9]{32}\/needtoken-api-staging@sha256:[0-9a-f]{64}$/

export function parseAllowlist(value) {
  return String(value || "")
    .split(",")
    .map((item) => item.trim().toLowerCase())
    .filter((item) => item !== "")
}

export function hostOnAllowlist(host, allowlist) {
  const name = String(host || "").trim().toLowerCase()
  if (name === "" || name.includes("*") || name.includes("/") || name.includes(",")) {
    return false
  }
  return allowlist.some((entry) => entry === name)
}

export function writerPermitAccepted(value) {
  const permit = String(value || "").trim()
  if (permit === "") {
    return false
  }
  return !TEST_PERMITS.has(permit.toLowerCase())
}

// attestedDigests returns the full registry image from a build record.
// A bare sha256, a hand-filled JSON document, or a SHA with no run is empty.
export function attestedDigests(env) {
  const source = env || {}
  const repository = text(source.NEEDTOKEN_BUILD_REPOSITORY)
  const sha = text(source.NEEDTOKEN_BUILD_SHA)
  const runId = text(source.NEEDTOKEN_BUILD_RUN_ID)
  const digest = text(source.NEEDTOKEN_BUILD_DIGEST)
  if (repository !== BUILD_REPOSITORY) {
    return []
  }
  if (!/^[0-9a-f]{40}$/.test(sha)) {
    return []
  }
  if (!/^[1-9][0-9]{5,}$/.test(runId)) {
    return []
  }
  if (digest.startsWith("{") || digest.startsWith("sha256:")) {
    return []
  }
  if (!FULL_IMAGE.test(digest) || digest.endsWith(PLACEHOLDER_DIGEST)) {
    return []
  }
  return [digest]
}

export function handFilledRecord(value) {
  if (value && typeof value === "object") {
    return true
  }
  const textValue = String(value || "").trim()
  if (textValue.startsWith("{") || textValue.startsWith("[")) {
    return true
  }
  return /^sha256:[0-9a-f]{64}$/.test(textValue)
}

function text(value) {
  return typeof value === "string" ? value.trim() : ""
}
