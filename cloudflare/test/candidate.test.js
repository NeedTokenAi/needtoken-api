import assert from "node:assert/strict"
import test from "node:test"
import { bindCandidate, lintRepo } from "../candidate/check.mjs"
import { PLACEHOLDER_DIGEST } from "../src/boot.js"

const SHA = "b33721ae32f468dba889071b9cbba252b27d7f0c"
const DIGEST = "sha256:" + "ab".repeat(32)

test("repository candidate files do not deploy", () => {
  const lint = lintRepo()
  assert.equal(lint.ok, true, lint.errors.join("\n"))
  assert.equal(lint.pinnedDigest, PLACEHOLDER_DIGEST)
})

test("binding requires current main, a real digest, and approval", () => {
  const pinned = DIGEST
  const good = {
    approval: "approved",
    mainSHA: SHA,
    headSHA: SHA,
    originMainSHA: SHA,
    imageDigest: DIGEST,
    pinnedDigest: pinned,
    workflowDeploys: false,
  }
  const source = {
    digest: DIGEST,
    sourceSHA: SHA,
    runId: "build-1",
    rollback: false,
    authorizesBoot: false,
  }
  const bound = bindCandidate({ ...good, buildSource: source })
  assert.equal(bound.ok, true)
  assert.equal(bound.deploy, false)
  assert.equal(bound.boot, false)
  assert.equal(bound.live, false)
  assert.equal(bound.rollback, false)
  assert.equal(bound.gcpFenced, false)
  assert.equal(bindCandidate(good).ok, false)
  assert.equal(bindCandidate({ ...good, buildSource: { ...source, authorizesBoot: true } }).ok, false)
  assert.equal(bindCandidate({ ...good, buildSource: { ...source, rollback: true } }).ok, false)
  assert.equal(bindCandidate({ ...good, buildSource: { ...source, sourceSHA: "" } }).ok, false)
  assert.equal(
    bindCandidate({
      ...good,
      buildSource: source,
      sqlDSN: "postgres://user:supersecret@ep-prod.neon.tech/needtoken",
    }).ok,
    false,
  )
  assert.equal(bindCandidate({ ...good, approval: "no", buildSource: source }).ok, false)
  assert.equal(bindCandidate({ ...good, originMainSHA: "a".repeat(40) }).ok, false)
  assert.equal(bindCandidate({ ...good, imageDigest: PLACEHOLDER_DIGEST, pinnedDigest: PLACEHOLDER_DIGEST }).ok, false)
  assert.equal(bindCandidate({ ...good, imageDigest: "sha256:" + "cd".repeat(32) }).ok, false)
  assert.equal(bindCandidate({ ...good, workflowDeploys: true }).ok, false)
})
