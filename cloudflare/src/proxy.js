// Forward a container response without reading it. SSE and WebSocket upgrades
// stay intact only when the caller returns this same response.
export function forwardUpstream(response) {
  return response
}

export function containerRequest(request) {
  const url = new URL(request.url)
  url.protocol = "http:"
  url.host = "container"
  const headers = new Headers(request.headers)
  headers.delete("host")
  const init = { method: request.method, headers }
  if (request.method !== "GET" && request.method !== "HEAD" && request.body) {
    init.body = request.body
    init.duplex = "half"
  }
  return new Request(url, init)
}
