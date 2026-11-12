import { readFileSync } from "node:fs"
import path from "node:path"
import { fileURLToPath } from "node:url"
import { PLACEHOLDER_DIGEST } from "../src/boot.js"

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
  const image = container && container.images && container.images.newapi && container.images.newapi.image
  if (!image || !image.startsWith("registry.cloudflare.com/") || !image.includes("@sha256:")) {
    errors.push("image must be a Cloudflare registry digest")
  }
  if (/pkg\.dev|gcr\.io|docker\.io/.test(JSON.stringify(wrangler))) {
    errors.push("image must not reference another registry")
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
  return { ok: errors.length === 0, errors, pinnedDigest: image ? image.slice(image.lastIndexOf("@") + 1) : "" }
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
  if (errors.length > 0) {
    return { ok: false, errors }
  }
  return { ok: true, deploy: false, mainSHA: input.mainSHA, imageDigest: input.imageDigest }
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
