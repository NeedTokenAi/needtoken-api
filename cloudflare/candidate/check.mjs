import { readFileSync } from "node:fs"
import path from "node:path"
import { fileURLToPath } from "node:url"
import { existsSync } from "node:fs"
import { imageReference } from "../src/boot.js"
import { attestedDigests, handFilledRecord } from "../src/gate.js"
import { recoveryState } from "../src/recovery.js"

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..")

export function parseJSONC(text) {
  const withoutBlock = text.replace(/\/\*[\s\S]*?\*\//g, "")
  const withoutLine = withoutBlock.replace(/^\s*\/\/.*$/gm, "")
  return JSON.parse(withoutLine)
}

export function workflowDeploys(text) {
  const patterns = [/wrangler\s+deploy/, /wrangler\s+versions\s+upload/, /\bgcloud\b/, /cloud\s+run/i, /^\s*push\s*:/m]
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
  if (existsSync(path.join(repo, "cloudflare/candidate/build-source.json"))) {
    errors.push("hand-filled build JSON is not a build record")
  }
  if (existsSync(path.join(repo, "cloudflareprofile/isolated_hosts.json"))) {
    errors.push("hardcoded host list is not the injected allowlist")
  }
  const source = readFileSync(path.join(repo, "cloudflare/src/controller.js"), "utf8")
  if (source.includes("unitTestStart")) {
    errors.push("unitTestStart is not a boot gate")
  }
  const staging = parseJSONC(readFileSync(path.join(repo, "cloudflare/wrangler.staging.jsonc"), "utf8"))
  if (!String(staging.name || "").endsWith("-staging")) {
    errors.push("staging worker name must end in -staging")
  }
  if (staging.workers_dev !== false || staging.routes || staging.route) {
    errors.push("staging worker must not publish a route")
  }
  const imageWorkflowPath = path.join(repo, ".github/workflows/cloudflare-staging-image.yml")
  if (existsSync(imageWorkflowPath)) {
    errors.push("the image build workflow belongs on its own pull request")
  }
  const wranglerText = readFileSync(wranglerPath, "utf8") + readFileSync(path.join(repo, "cloudflare/wrangler.staging.jsonc"), "utf8")
  if (/postgres:\/\/|redis:\/\/|mysql:\/\/|BEGIN PRIVATE|AKIA/.test(wranglerText)) {
    errors.push("wrangler file contains a credential")
  }
  if (/"r2_buckets"/.test(wranglerText)) {
    errors.push("this service does not use R2")
  }
  const runtimeSources = [
    "cloudflare/src/boot.js",
    "cloudflare/src/gate.js",
    "cloudflareprofile/profile.go",
  ].map((name) => readFileSync(path.join(repo, name), "utf8")).join("\n")
  if (!runtimeSources.includes("DATABASE_HOST") || !runtimeSources.includes("ep-aged-sunset-b33os2qw.c-4.ap-southeast-1.aws.neon.tech")) {
    errors.push("staging DATABASE_HOST allowlist is missing")
  }
  if (runtimeSources.includes("NEEDTOKEN_HOST_ALLOWLIST")) {
    errors.push("Redis and arbitrary hosts are not the staging database allowlist")
  }
  if (workflowDeploys(workflow)) {
    errors.push("candidate workflow deploys or follows a push trigger")
  }
  if (!workflow.includes("check.mjs --lint")) {
    errors.push("workflow is missing check.mjs --lint")
  }
  if (workflow.includes("workflow_dispatch") || workflow.includes("CANDIDATE_IMAGE_DIGEST")) {
    errors.push("candidate workflow accepts a hand-filled digest")
  }
  if (workflow.includes("secrets.")) {
    errors.push("workflow reads repository secrets")
  }
  return {
    ok: errors.length === 0,
    errors,
    pinnedDigest: image ? image.slice(image.lastIndexOf("@") + 1) : "",
    previousDigest: previous ? previous.slice(previous.lastIndexOf("@") + 1) : "",
  }
}

function pinnedCloudflareDigest(image) {
  return typeof image === "string" && image.startsWith("registry.cloudflare.com/") && /@sha256:[0-9a-f]{64}$/.test(image)
}

export function bindCandidate(input) {
  if (handFilledRecord(input.buildSource) || handFilledRecord(input.imageDigest) || handFilledRecord(input.json)) {
    return { ok: false, boot: false, deploy: false, rollback: false, live: false, errors: ["hand-filled SHA or JSON is not a build record"] }
  }
  const attested = attestedDigests(input.env || {})
  const image = imageReference(input.images && input.images.newapi)
  if (attested.length !== 1 || attested[0] !== image) {
    return { ok: false, boot: false, deploy: false, rollback: false, live: false, attestedDigests: attested, errors: ["image is not an attested build"] }
  }
  return { ok: true, boot: true, deploy: false, live: false, rollback: false, gcpFenced: false, attestedDigests: attested }
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
  return fail("hand-filled SHA or JSON is not a build record")
}

if (process.argv[1] && fileURLToPath(import.meta.url) === path.resolve(process.argv[1])) {
  process.exit(main())
}
