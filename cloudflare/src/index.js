import { DurableObject } from "cloudflare:workers";
import {
  APP_PORT,
  INACTIVITY_TIMEOUT_MS,
  containerEnv,
  containerRequest,
  residentStub,
  waitForPort,
} from "./forward.js";

export class NeedtokenApiContainer extends DurableObject {
  constructor(ctx, env) {
    super(ctx, env);
    this.starting = undefined;
    const container = ctx.container;
    if (container?.running) {
      void ctx.blockConcurrencyWhile(() => container.setInactivityTimeout(INACTIVITY_TIMEOUT_MS));
    }
  }

  async fetch(request) {
    await this.ensureStarted();
    return this.ctx.container.getTcpPort(APP_PORT).fetch(containerRequest(request));
  }

  async ensureStarted() {
    const container = this.ctx.container;
    if (container.running) {
      await container.setInactivityTimeout(INACTIVITY_TIMEOUT_MS);
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
      await container.setInactivityTimeout(INACTIVITY_TIMEOUT_MS);
      return;
    }
    // Image and instance size come from wrangler.jsonc. enableInternet is
    // required when options are passed. This process calls Neon, Upstash,
    // and upstream APIs.
    container.start({
      enableInternet: true,
      env: containerEnv(this.env),
    });
    await container.setInactivityTimeout(INACTIVITY_TIMEOUT_MS);
    await waitForPort(
      () => container.getTcpPort(APP_PORT).fetch("http://container/"),
      (ms) => scheduler.wait(ms),
    );
  }
}

export default {
  async fetch(request, env) {
    return residentStub(env.NEEDTOKEN_API).fetch(request);
  },

  async scheduled(_controller, env) {
    await residentStub(env.NEEDTOKEN_API).fetch("https://needtoken-api/api/status");
  },
};
