export const SINGLETON_NAME = "needtoken-api"

export const secretBindings = [
  { secret: "SQL_DSN", binding: "SQL_DSN", required: true },
  { secret: "SESSION_SECRET", binding: "SESSION_SECRET", required: true },
  { secret: "REDIS_CONN_STRING", binding: "REDIS_CONN_STRING", required: true },
  { secret: "CRYPTO_SECRET", binding: "CRYPTO_SECRET", required: true },
  { secret: "LOG_SQL_DSN", binding: "LOG_SQL_DSN", explicit: "main-or-separate" },
]

// routeSingleton ignores every instance name carried by the request.
// One Durable Object name is the code-enforced singleton. The durable_object
// scheduling policy does not provide max_instances.
export function routeSingleton(namespace, request) {
  void request
  if (!namespace || typeof namespace.getByName !== "function") {
    throw new Error("singleton binding is absent")
  }
  return namespace.getByName(SINGLETON_NAME)
}
