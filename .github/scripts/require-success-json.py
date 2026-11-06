#!/usr/bin/env python3
"""Exit 0 only when a response body is a JSON object with success == true."""

import json
import sys


def main() -> int:
    if len(sys.argv) != 2:
        print("usage: require-success-json.py FILE", file=sys.stderr)
        return 1
    with open(sys.argv[1], "rb") as handle:
        raw = handle.read()
    try:
        data = json.loads(raw)
    except json.JSONDecodeError:
        print("response is not JSON", file=sys.stderr)
        return 1
    if not isinstance(data, dict) or data.get("success") is not True:
        print("success is not true", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
