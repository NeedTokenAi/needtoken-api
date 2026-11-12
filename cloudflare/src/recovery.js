import { imageDigest, imageReference } from "./boot.js"

// recoveryState records the current and previous digests. It does not switch
// images. Two digests are not a rollback, and this skeleton does not claim one.
export function recoveryState(images) {
  const current = imageDigest(imageReference(images && images.newapi))
  const previous = imageDigest(imageReference(images && images.previous))
  return {
    current,
    previous,
    selected: current,
    switchesImage: false,
    rollback: false,
    reason: "two digests are recorded; this skeleton does not roll back",
  }
}
