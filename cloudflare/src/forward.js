// App port the Worker dials. The container is given the same port below.
export const APP_PORT = 8080;

// One Durable Object name. max_instances 2 would allow a second container,
// and each container runs the in-process loops. Only this name is used.
export const INSTANCE_NAME = "instance-0";

// setInactivityTimeout rejects a duration above 6 hours. 24h is not accepted.
// The minute cron hits this instance so the timeout does not expire.
export const INACTIVITY_TIMEOUT_MS = 6 * 60 * 60 * 1000;

// One probe. A closed port returns 503 instead of blocking the caller.
// The cron retries that 503 on the same instance until this deadline.
// After a Durable Object rebuild the container may already be running, and
// its first response can take longer than one second. This is that budget.
export const PROBE_ATTEMPTS = 1;
export const PROBE_TIMEOUT_MS = 5000;
export const RETRY_AFTER_SECONDS = 5;
export const READY_DEADLINE_MS = 4 * 60 * 1000;

export function residentStub(namespace) {
  if (!namespace || typeof namespace.getByName !== "function") {
    throw new Error("container binding is absent");
  }
  return namespace.getByName(INSTANCE_NAME);
}

// Copy Worker vars and secrets as strings. Bindings that are not strings are
// omitted because container env values must be strings. Other values are not
// rewritten, including an empty Redis prefix. PORT is set to 8080 because
// Cloud Run injects it and the production env does not include it.
export function containerEnv(env) {
  const forwarded = {};
  if (env && typeof env === "object") {
    for (const [key, value] of Object.entries(env)) {
      if (typeof value === "string") {
        forwarded[key] = value;
      }
    }
  }
  forwarded.PORT = "8080";
  return forwarded;
}

// Rewrite the URL to http://container and copy the original Request so the
// body stream, cancellation signal, and upgrade are kept.
export function containerRequest(request) {
  const url = new URL(request.url);
  url.protocol = "http:";
  url.host = "container";
  const forwarded = new Request(url, request);
  forwarded.headers.delete("host");
  return forwarded;
}

export function containerNotReadyError(cause) {
  const error = new Error("container did not become ready on port 8080");
  error.name = "ContainerNotReadyError";
  if (cause !== undefined) {
    error.cause = cause;
  }
  return error;
}

export function isContainerNotReady(error) {
  if (!error || typeof error !== "object") {
    return false;
  }
  if (error.name === "ContainerNotReadyError") {
    return true;
  }
  return typeof error.message === "string" && error.message.includes("did not become ready");
}

export function notReadyResponse() {
  return new Response("container did not become ready on port 8080", {
    status: 503,
    headers: {
      "retry-after": String(RETRY_AFTER_SECONDS),
      "cache-control": "no-store",
    },
  });
}

export function startupFailureResponse(error) {
  if (!isContainerNotReady(error)) {
    return undefined;
  }
  return notReadyResponse();
}

// start() returns before the process listens. Any HTTP response means the
// port accepted a connection. The response body is not read.
export async function waitForPort(fetchPort, wait, attempts = PROBE_ATTEMPTS) {
  let lastError;
  for (let attempt = 0; attempt < attempts; attempt += 1) {
    try {
      const response = await fetchPort();
      if (response?.body && typeof response.body.cancel === "function") {
        await response.body.cancel();
      }
      return;
    } catch (error) {
      lastError = error;
    }
    if (attempt + 1 < attempts) {
      await wait(200);
    }
  }
  throw containerNotReadyError(lastError);
}

// Retry a 503 from the one routed instance. Other statuses are returned as-is.
export async function awaitInstance(fetchInstance, wait, deadlineMs = READY_DEADLINE_MS, now = Date.now) {
  const started = now();
  let response = await fetchInstance();
  while (response.status === 503 && now() - started < deadlineMs) {
    if (response.body && typeof response.body.cancel === "function") {
      await response.body.cancel();
    }
    await wait(RETRY_AFTER_SECONDS * 1000);
    response = await fetchInstance();
  }
  return response;
}
