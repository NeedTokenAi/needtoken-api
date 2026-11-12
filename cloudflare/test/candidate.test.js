import assert from "node:assert/strict"
import test from "node:test"
import { bindCandidate, lintRepo } from "../candidate/check.mjs"
import { PLACEHOLDER_DIGEST } from "../src/boot.js"

const SHA = "b33721ae32f468dba889071b9cbba252b27d7f0c"
const ACCOUNT = "a".repeat(32)
const DIGEST = "sha256:" + "ab".repeat(32)
const IMAGE = "registry.cloudflare.com/" + ACCOUNT + "/needtoken-api-staging@" + DIGEST

test("repository candidate files do not deploy", () => {
  const lint = lintRepo()
  assert.equal(lint.ok, true, lint.errors.join("\n"))
  assert.equal(lint.pinnedDigest, PLACEHOLDER_DIGEST)
})

test("a hand-filled SHA or JSON is not a build record", () => {
  const env = {
    NEEDTOKEN_BUILD_REPOSITORY: "NeedTokenAi/needtoken-api",
    NEEDTOKEN_BUILD_SHA: SHA,
    NEEDTOKEN_BUILD_RUN_ID: "43174047407",
    NEEDTOKEN_BUILD_DIGEST: IMAGE,
  }
  const images = { newapi: IMAGE }
  const bound = bindCandidate({ env, images })
  assert.equal(bound.boot, true)
  assert.equal(bound.deploy, false)
  assert.equal(bound.live, false)
  assert.equal(bound.rollback, false)
  assert.deepEqual(bound.attestedDigests, [IMAGE])
  assert.equal(bindCandidate({ imageDigest: DIGEST, buildSource: { sourceSHA: SHA, digest: DIGEST, runId: "1" } }).ok, false)
  assert.equal(bindCandidate({ json: JSON.stringify({ digest: IMAGE, sourceSHA: SHA }) }).ok, false)
  assert.equal(bindCandidate({ env: { ...env, NEEDTOKEN_BUILD_DIGEST: DIGEST }, images }).ok, false)
  assert.equal(bindCandidate({ env, images: { newapi: DIGEST } }).ok, false)
})
