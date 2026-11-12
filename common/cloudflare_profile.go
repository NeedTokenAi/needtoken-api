package common

import "os"

// CloudflareProfileRequested reports the default-off Cloudflare runtime
// profile. Only the exact value "1" enables it. Production stays on the
// existing runtime when the variable is unset.
func CloudflareProfileRequested() bool {
	return os.Getenv("CLOUDFLARE_PROFILE") == "1"
}

// UseStdoutLogsForCloudflareProfile drops the log directory when the profile
// is on, including an explicit --log-dir. File logs under the container's
// temporary disk must not be the only copy; the process keeps writing to
// stdout and stderr.
func UseStdoutLogsForCloudflareProfile() {
	if CloudflareProfileRequested() {
		*LogDir = ""
	}
}
