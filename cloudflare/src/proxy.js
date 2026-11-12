// containerRequest follows the current Containers fetch example:
// rewrite the URL to http://container and construct the request from the
// original Request so its body stream, cancellation signal, and upgrade are
// kept. The host header is removed after that copy.
export function containerRequest(request) {
  const url = new URL(request.url)
  url.protocol = "http:"
  url.host = "container"
  const forwarded = new Request(url, request)
  forwarded.headers.delete("host")
  return forwarded
}

// forwardUpstream returns the container response itself. SSE is not read
// or buffered here.
export function forwardUpstream(response) {
  return response
}
