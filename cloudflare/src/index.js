import { DurableObject } from "cloudflare:workers";
import { APP_PORT, containerEnv, containerRequest, waitForPort } from "./forward.js";

const INSTANCE_NAME = "needtoken-api";

export class NeedtokenApiContainer extends DurableObject {
  constructor(ctx, env) {
    super(ctx, env);
    this.starting = undefined;
  }

  async fetch(request) {
    await this.ensureStarted();
    return this.ctx.container.getTcpPort(APP_PORT).fetch(containerRequest(request));
  }

  async ensureStarted() {
    const container = this.ctx.container;
    if (container.running) {
      return;
    }
    this.starting ??= this.startContainer().finally(() => {
      this.starting = undefined;
    });
    await this.starting;
  }

  async startContainer() {
    const container = this.ctx.container;
    if (container.running) {
      return;
    }
    // Image and instance size come from wrangler.jsonc. Env is the Worker
    // bindings copied unchanged. enableInternet is required when options
    // are passed, and this process calls Neon, Upstash, and upstream APIs.
    container.start({
      enableInternet: true,
      env: containerEnv(this.env),
    });
    await waitForPort(
      () => container.getTcpPort(APP_PORT).fetch("http://container/"),
      (ms) => scheduler.wait(ms),
    );
  }
}

export default {
  async fetch(request, env) {
    return env.NEEDTOKEN_API.getByName(INSTANCE_NAME).fetch(request);
  },
};
