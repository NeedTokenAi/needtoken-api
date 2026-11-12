import { DurableObject } from "cloudflare:workers"
import { createController } from "./controller.js"
import { routeSingleton, SINGLETON_NAME } from "./singleton.js"

export class NewAPIContainer extends DurableObject {
  constructor(ctx, env) {
    super(ctx, env)
    this.controller = createController({
      env,
      images: ctx.container && ctx.container.images,
      storage: ctx.storage,
      container: ctx.container,
    })
    void ctx.blockConcurrencyWhile(() => this.controller.hydrate())
  }

  async fetch(request) {
    return this.controller.onFetch(request)
  }

  async alarm() {
    await this.controller.onAlarm()
  }
}

export default {
  async fetch(request, env) {
    return routeSingleton(env.NEWAPI_CONTAINER, request).fetch(request)
  },
}

export { SINGLETON_NAME }
