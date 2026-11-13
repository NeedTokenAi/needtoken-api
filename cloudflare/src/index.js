import { DurableObject, WorkerEntrypoint } from "cloudflare:workers";
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
import {
  armPoolIntercept,
  denyInternal,
  internalUnavailableResponse,
  routeInternal,
} from "./internal.js";

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
      const unavailable = internalUnavailableResponse(error);
      if (unavailable) {
        return unavailable;
      }
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
    // A missing or illegal INTERNAL_POOL_HOST, or an intercept error, throws
    // before start. The channel proxy host is not part of this registration.
    const armed = await armPoolIntercept(container, this.env, this.ctx.exports);
    if (!container.running) {
      // Image and instance size come from wrangler.jsonc. enableInternet is
      // required when options are passed. This process calls Neon, Upstash,
      // and upstream APIs. The entrypoint trusts the intercept CA as well as
      // the image bundle, then execs /new-api.
      container.start({
        enableInternet: true,
        env: containerEnv(this.env),
        entrypoint: armed.entrypoint,
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

export class InternalDeny extends WorkerEntrypoint {
  fetch() {
    return denyInternal();
  }
}

export class InternalHop extends WorkerEntrypoint {
  fetch(request) {
    return routeInternal(
      request,
      this.env,
      this.ctx.props,
      (binding, forwarded, init) => binding.fetch(forwarded, init),
    );
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
