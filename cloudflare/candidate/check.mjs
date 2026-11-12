import { readFileSync } from "node:fs"
import path from "node:path"
import { fileURLToPath } from "node:url"
import { ISOLATED_SQL_HOSTS, PLACEHOLDER_DIGEST, imageReference, isolatedSQLHost } from "../src/boot.js"
import { recoveryState } from "../src/recovery.js"

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..")

export function parseJSONC(text) {
  const withoutBlock = text.replace(/\/\*[\s\S]*?\*\//g, "")
  const withoutLine = withoutBlock.replace(/^\s*\/\/.*$/gm, "")
  return JSON.parse(withoutLine)
}

export function workflowDeploys(text) {
  const patterns = [/wrangler\s+deploy/, /npx\s+wrangler/, /\bgcloud\b/, /cloud\s+run/i, /^\s*push\s*:/m]
  return patterns.some((pattern) => pattern.test(text))
}

export function lintRepo(repo = root) {
  const errors = []
  const wranglerPath = path.join(repo, "cloudflare/wrangler.jsonc")
  const workflowPath = path.join(repo, ".github/workflows/cloudflare-candidate.yml")
  const wrangler = parseJSONC(readFileSync(wranglerPath, "utf8"))
  const workflow = readFileSync(workflowPath, "utf8")
  const container = wrangler.containers && wrangler.containers[0]
  if (!container || container.scheduling_policy !== "durable_object") {
    errors.push("scheduling_policy must be durable_object")
  }
  if (container && Object.hasOwn(container, "max_instances")) {
    errors.push("durable_object policy must not set max_instances")
  }
  const images = container && container.images
  const image = imageReference(images && images.newapi)
  const previous = imageReference(images && images.previous)
  if (!pinnedCloudflareDigest(image) || !pinnedCloudflareDigest(previous)) {
    errors.push("image must be a Cloudflare registry digest")
  }
  if (image && previous && image === previous) {
    errors.push("previous digest must be a distinct recovery record")
  }
  const recovery = recoveryState({
    newapi: images && images.newapi,
    previous: images && images.previous,
  })
  if (recovery.rollback !== false || recovery.switchesImage !== false) {
    errors.push("recovery state must not claim a rollback")
  }
  if (/pkg\.dev|gcr\.io|docker\.io/.test(JSON.stringify(wrangler))) {
    errors.push("image must not reference another registry")
  }
  const uses = workflow.match(/^[ \t]*uses:[ \t]*(\S+)/gm) || []
  if (uses.length === 0) {
    errors.push("workflow does not pin actions")
  }
  for (const line of uses) {
    if (!/@[0-9a-f]{40}\b/.test(line)) {
      errors.push("action is not pinned to a commit SHA: " + line.trim())
    }
  }
  const buildSource = JSON.parse(readFileSync(path.join(repo, "cloudflare/candidate/build-source.json"), "utf8"))
  if (buildSource.authorizesBoot !== false || buildSource.rollback !== false) {
    errors.push("build source must stay fail-closed")
  }
  if (buildSource.digest !== image.slice(image.lastIndexOf("@") + 1)) {
    errors.push("build source digest is not the pinned candidate")
  }
  const allowlist = JSON.parse(readFileSync(path.join(repo, "cloudflareprofile/isolated_hosts.json"), "utf8"))
  if (!allowlist.hosts || allowlist.hosts.join(",") !== ISOLATED_SQL_HOSTS.join(",")) {
    errors.push("isolated SQL allowlist drifted")
  }
  if (allowlist.hosts.some((host) => /neon\.tech|googleapis|cloudsql/i.test(host))) {
    errors.push("isolated SQL allowlist contains a production host")
  }
  if (/postgres:\/\/|redis:\/\/|mysql:\/\/|BEGIN PRIVATE|AKIA/.test(readFileSync(wranglerPath, "utf8"))) {
    errors.push("wrangler file contains a credential")
  }
  if (workflowDeploys(workflow)) {
    errors.push("candidate workflow deploys or follows a push trigger")
  }
  for (const required of ["workflow_dispatch", "approval", "cloudflare-candidate-approval", "check.mjs --lint", "check.mjs --bind"]) {
    if (!workflow.includes(required)) {
      errors.push("workflow is missing " + required)
    }
  }
  if (workflow.includes("secrets.")) {
    errors.push("workflow reads repository secrets")
  }
  return {
    ok: errors.length === 0,
    errors,
    pinnedDigest: image ? image.slice(image.lastIndexOf("@") + 1) : "",
    previousDigest: previous ? previous.slice(previous.lastIndexOf("@") + 1) : "",
    buildSource,
  }
}

function pinnedCloudflareDigest(image) {
  return typeof image === "string" && image.startsWith("registry.cloudflare.com/") && /@sha256:[0-9a-f]{64}$/.test(image)
}

export function bindCandidate(input) {
  const errors = []
  if (input.approval !== "approved") {
    errors.push("approval gate rejected")
  }
  if (!/^[0-9a-f]{40}$/.test(input.mainSHA || "")) {
    errors.push("main SHA is not a commit")
  }
  if (input.mainSHA !== input.headSHA || input.mainSHA !== input.originMainSHA) {
    errors.push("main SHA is not the current origin/main")
  }
  if (!/^sha256:[0-9a-f]{64}$/.test(input.imageDigest || "")) {
    errors.push("image digest is not pinned")
  }
  if (input.imageDigest === PLACEHOLDER_DIGEST) {
    errors.push("placeholder digest")
  }
  if (input.imageDigest !== input.pinnedDigest) {
    errors.push("image digest is not the pinned candidate")
  }
  if (input.workflowDeploys) {
    errors.push("candidate check refuses to deploy")
  }
  const source = input.buildSource || {}
  if (source.sourceSHA !== input.mainSHA) {
    errors.push("digest is not tied to the main SHA")
  }
  if (source.digest !== input.imageDigest) {
    errors.push("digest is not the recorded build source")
  }
  if (!source.runId) {
    errors.push("build run is absent")
  }
  if (source.rollback !== false) {
    errors.push("rollback is not available")
  }
  if (source.authorizesBoot !== false) {
    errors.push("this skeleton cannot authorize boot")
  }
  if (input.sqlDSN && !isolatedSQLHost(input.sqlDSN)) {
    errors.push("SQL host is outside the isolated allowlist")
  }
  if (input.sqlHost && !ISOLATED_SQL_HOSTS.includes(input.sqlHost)) {
    errors.push("SQL host is outside the isolated allowlist")
  }
  if (errors.length > 0) {
    return { ok: false, errors }
  }
  return {
    ok: true,
    deploy: false,
    boot: false,
    live: false,
    rollback: false,
    gcpFenced: false,
    mainSHA: input.mainSHA,
    imageDigest: input.imageDigest,
  }
}

function fail(message) {
  console.error(message)
  return 1
}

function main() {
  const lint = lintRepo()
  if (process.argv.includes("--lint")) {
    if (!lint.ok) {
      return fail(lint.errors.join("\n"))
    }
    console.log("cloudflare candidate lint ok")
    return 0
  }
  if (!process.argv.includes("--bind")) {
    return fail("use --lint or --bind")
  }
  if (!lint.ok) {
    return fail(lint.errors.join("\n"))
  }
  const bound = bindCandidate({
    approval: process.env.CANDIDATE_APPROVAL,
    mainSHA: process.env.CANDIDATE_MAIN_SHA,
    headSHA: process.env.CANDIDATE_HEAD_SHA,
    originMainSHA: process.env.CANDIDATE_ORIGIN_MAIN_SHA,
    imageDigest: process.env.CANDIDATE_IMAGE_DIGEST,
    pinnedDigest: lint.pinnedDigest,
    workflowDeploys: false,
    buildSource: lint.buildSource,
  })
  if (!bound.ok) {
    return fail(bound.errors.join("\n"))
  }
  console.log("candidate main_sha=" + bound.mainSHA)
  console.log("candidate image_digest=" + bound.imageDigest)
  console.log("deploy=false")
  return 0
}

if (process.argv[1] && fileURLToPath(import.meta.url) === path.resolve(process.argv[1])) {
  process.exit(main())
}
