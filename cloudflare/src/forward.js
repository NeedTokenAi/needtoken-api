// App port the Worker dials. Production PORT is passed through unchanged.
export const APP_PORT = 8080;

// Copy Worker vars and secrets as strings. Bindings that are not strings are
// omitted because container env values must be strings. Nothing is added,
// removed, or rewritten, including an empty Redis prefix.
export function containerEnv(env) {
  const forwarded = {};
  if (!env || typeof env !== "object") {
    return forwarded;
  }
  for (const [key, value] of Object.entries(env)) {
    if (typeof value === "string") {
      forwarded[key] = value;
    }
  }
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

// start() returns before the process listens. Any HTTP response means the
// port accepted a connection. The response body is not read.
export async function waitForPort(fetchPort, wait, attempts = 100) {
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
  const error = new Error("container port 8080 did not accept a connection");
  error.cause = lastError;
  throw error;
}
