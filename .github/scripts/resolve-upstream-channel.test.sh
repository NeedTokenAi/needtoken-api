#!/usr/bin/env bash
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
script="$root/resolve-upstream-channel.sh"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

assert_eq() {
  local got="$1" want="$2" label="$3"
  [[ "$got" == "$want" ]] || fail "$label: got [$got] want [$want]"
}

tags=$(printf '%s\n' \
  v0.13.2 \
  v1.0.0-alpha.1 \
  v1.0.0-rc.2 \
  v1.0.0-rc.9 \
  v1.0.0-rc.10 \
  v1.0.0-rc.19-i18nfix.2 \
  v1.0.0-rc.48 \
  nightly-20260409 \
  v1X0X0-rc.99)

got=$(printf '%s\n' "$tags" | UPSTREAM_CHANNEL=v1.0.0-rc bash "$script" select-tag)
assert_eq "$got" "v1.0.0-rc.48" "highest rc, not stable or i18nfix"

got=$(printf '%s\n' "$tags" | env -u UPSTREAM_CHANNEL bash "$script" select-tag)
assert_eq "$got" "v1.0.0-rc.48" "default channel is v1.0.0-rc"

got=$(printf '%s\n' 'v1.0.0-rc.48' 'v1.1.0-rc.2' 'v1.1.0-rc.10' | UPSTREAM_CHANNEL=v1.1.0-rc bash "$script" select-tag)
assert_eq "$got" "v1.1.0-rc.10" "other rc series does not fall back"

got=$(printf '%s\n' 'v1.0.0-rc.08' 'v1.0.0-rc.7' | UPSTREAM_CHANNEL=v1.0.0-rc bash "$script" select-tag)
assert_eq "$got" "v1.0.0-rc.08" "leading zero is decimal 8"

got=$(printf '%s\n' 'v1.0.0-rc.08' 'v1.0.0-rc.8' | UPSTREAM_CHANNEL=v1.0.0-rc bash "$script" select-tag)
assert_eq "$got" "v1.0.0-rc.8" "canonical tag wins a numeric tie"

got=$(printf '%s\r\n' 'v1.0.0-rc.3' | UPSTREAM_CHANNEL=v1.0.0-rc bash "$script" select-tag)
assert_eq "$got" "v1.0.0-rc.3" "crlf tag"

got=$(printf '%s\n' 'v1.0.0-rc.4' | UPSTREAM_CHANNEL= bash "$script" select-tag)
assert_eq "$got" "v1.0.0-rc.4" "empty channel uses the default RC series"

for bad in v0.13 stable latest v1.0.0 v1.0.0-rc.48 v1.0.0-alpha; do
  if printf '%s\n' 'v0.13.2' 'v1.0.0-rc.1' | UPSTREAM_CHANNEL="$bad" bash "$script" select-tag >/dev/null 2>&1; then
    fail "channel [$bad] should be rejected"
  fi
done

if printf '' | UPSTREAM_CHANNEL=v1.0.0-rc bash "$script" select-tag >/dev/null 2>&1; then
  fail "empty tag list should fail"
fi

peeled=$(printf '%s\n' \
  'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa refs/tags/v1.0.0-rc.48' \
  'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb refs/tags/v1.0.0-rc.48^{}' | bash "$script" peel-sha)
assert_eq "$peeled" "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" "annotated tag peels"

light=$(printf '%s\n' '706144769e5bed5a47b78721e3bb11da3cf4c0b9 refs/tags/v1.0.0-rc.48' | bash "$script" peel-sha)
assert_eq "$light" "706144769e5bed5a47b78721e3bb11da3cf4c0b9" "lightweight tag"

if printf '%s\n' 'not-a-sha refs/tags/v1.0.0-rc.48' | bash "$script" peel-sha >/dev/null 2>&1; then
  fail "garbage ls-remote should fail"
fi
if printf '%s\n' \
  'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa refs/tags/v1.0.0-rc.48' \
  'bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb refs/tags/v1.0.0-rc.47' | bash "$script" peel-sha >/dev/null 2>&1; then
  fail "two tag lines without a peel should fail"
fi

echo "resolve-upstream-channel.test.sh ok"
