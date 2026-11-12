import { DurableObject } from "cloudflare:workers";
import {
  APP_PORT,
  INACTIVITY_TIMEOUT_MS,
  PROBE_ATTEMPTS,
  awaitInstance,
  containerEnv,
  containerNotReadyError,
  containerRequest,
  residentStub,
  startupFailureResponse,
  waitForPort,
} from "./forward.js";

export class NeedtokenApiContainer extends DurableObject {
  constructor(ctx, env) {
    super(ctx, env);
    this.starting = undefined;
    this.portReady = false;
    const container = ctx.container;
    if (container?.running) {
      void ctx.blockConcurrencyWhile(() => container.setInactivityTimeout(INACTIVITY_TIMEOUT_MS));
    }
  }

  async fetch(request) {
    try {
      await this.ensureStarted();
      return await this.ctx.container.getTcpPort(APP_PORT).fetch(containerRequest(request));
    } catch (error) {
      this.portReady = false;
      const response = startupFailureResponse(error);
      if (response) {
        return response;
      }
      throw error;
    }
  }

  async ensureStarted() {
    const container = this.ctx.container;
    if (this.portReady && container.running) {
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
    if (!container.running) {
      // Image and instance size come from wrangler.jsonc. enableInternet is
      // required when options are passed. This process calls Neon, Upstash,
      // and upstream APIs.
      container.start({
        enableInternet: true,
        env: containerEnv(this.env),
      });
    }
    await container.setInactivityTimeout(INACTIVITY_TIMEOUT_MS);
    await waitForPort(
      () => container.getTcpPort(APP_PORT).fetch("http://container/", {
        signal: AbortSignal.timeout(1000),
      }),
      (ms) => scheduler.wait(ms),
      PROBE_ATTEMPTS,
    );
    this.portReady = true;
  }
}

export default {
  async fetch(request, env) {
    return residentStub(env.NEEDTOKEN_API).fetch(request);
  },

  async scheduled(_controller, env) {
    const response = await awaitInstance(
      () => residentStub(env.NEEDTOKEN_API).fetch("https://needtoken-api/api/status"),
      (ms) => scheduler.wait(ms),
    );
    if (response.body && typeof response.body.cancel === "function") {
      await response.body.cancel();
    }
    if (response.status === 503) {
      throw containerNotReadyError();
    }
  },
};
