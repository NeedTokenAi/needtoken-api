// Outbound hop from this container to needtoken-pool.
// The host comes only from the INTERNAL_POOL_HOST secret. A channel proxy
// host is never registered. Failure returns 502/503 and does not call fetch.

export const POOL_BINDING = "INTERNAL_POOL";
export const POOL_HOST_SECRET = "INTERNAL_POOL_HOST";

// Platform default from Containers limits, updated 2026-11-12. There is no
// Worker, wrangler, or intercept argument that raises it. A 413 stays 413.
export const OUTBOUND_BODY_LIMIT_BYTES = 100 * 1024 * 1024;

const HOSTNAME = /^(?=.{1,253}$)[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)*$/;
const IPV4 = /^(?:\d{1,3}\.){3}\d{1,3}$/;

export function isSingleDnsHostname(value) {
  return typeof value === "string" && HOSTNAME.test(value) && !IPV4.test(value);
}

export function internalUnavailableError(cause) {
  const error = new Error("internal channel unavailable");
  error.name = "InternalChannelUnavailable";
  if (cause !== undefined) {
    error.cause = cause;
  }
  return error;
}

export function internalUnavailableResponse(error) {
  if (!error || error.name !== "InternalChannelUnavailable") {
    return undefined;
  }
  return new Response("internal channel unavailable", {
    status: 503,
    headers: { "cache-control": "no-store" },
  });
}

export function internalFailedResponse() {
  return new Response("internal channel failed", {
    status: 502,
    headers: { "cache-control": "no-store" },
  });
}

export function denyInternal() {
  return new Response("internal channel denied", {
    status: 403,
    headers: { "cache-control": "no-store" },
  });
}

export function requirePoolHost(env) {
  const value = env?.[POOL_HOST_SECRET];
  if (!isSingleDnsHostname(value)) {
    throw internalUnavailableError();
  }
  return value;
}

function shellQuote(value) {
  return `'${String(value).replaceAll("'", `'\\''`)}'`;
}

// Concatenate the image CA bundle with the runtime intercept CA, then exec.
// Paths default to the container locations. Tests pass temporary paths.
export function trustScript(paths = {}) {
  const primary = paths.primary ?? "/etc/ssl/certs/ca-certificates.crt";
  const fallback = paths.fallback ?? "/etc/ssl/cert.pem";
  const cloudflare = paths.cloudflare ?? "/etc/cloudflare/certs/cloudflare-containers-ca.crt";
  const bundle = paths.bundle ?? "/tmp/needtoken-ca.pem";
  const command = paths.command ?? "/new-api";
  return [
    "set -eu",
    `src=${shellQuote(primary)}`,
    `if [ ! -r "$src" ]; then src=${shellQuote(fallback)}; fi`,
    'test -r "$src"',
    `test -r ${shellQuote(cloudflare)}`,
    `cat "$src" ${shellQuote(cloudflare)} > ${shellQuote(bundle)}`,
    `export SSL_CERT_FILE=${shellQuote(bundle)}`,
    `exec ${shellQuote(command)}`,
  ].join("\n");
}

export function trustEntrypoint() {
  return ["sh", "-lc", trustScript()];
}

export async function registerPoolIntercepts(container, exportsApi, host) {
  await container.interceptOutboundHttp(host, exportsApi.InternalDeny({ props: { host } }));
  await container.interceptOutboundHttps(
    host,
    exportsApi.InternalHop({ props: { host, binding: POOL_BINDING } }),
  );
}

// Register the one pool host. Does not call container.start.
export async function armPoolIntercept(container, env, exportsApi) {
  const host = requirePoolHost(env);
  try {
    await registerPoolIntercepts(container, exportsApi, host);
  } catch (error) {
    if (error?.name === "InternalChannelUnavailable") {
      throw error;
    }
    throw internalUnavailableError(error);
  }
  return { host, entrypoint: trustEntrypoint() };
}

function denied() {
  return denyInternal();
}

export async function routeInternal(request, env, props, fetchBinding) {
  if (props?.binding !== POOL_BINDING) {
    return denied();
  }
  let url;
  try {
    url = new URL(request.url);
  } catch {
    return denied();
  }
  if (
    url.protocol !== "https:"
    || url.port !== ""
    || url.username !== ""
    || url.password !== ""
    || url.hostname !== props.host
  ) {
    return denied();
  }
  const binding = env?.[props.binding];
  if (!binding || typeof binding.fetch !== "function") {
    return internalFailedResponse();
  }
  let forwarded;
  try {
    forwarded = new Request(url, request);
  } catch {
    return denied();
  }
  forwarded.headers.delete("Cloudflare-Workers-Version-Overrides");
  try {
    return await fetchBinding(binding, forwarded, { redirect: "manual" });
  } catch {
    return internalFailedResponse();
  }
}
