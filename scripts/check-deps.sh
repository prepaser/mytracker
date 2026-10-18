#!/bin/sh
set -eu

module="$(go list -m)"
tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT
go list -f '{{.ImportPath}} {{join .Imports " "}}' ./internal/... >"$tmp"
violations="$(
	awk -v module="$module" '
		$1 == module "/internal/app" ||
		$1 == module "/internal/web" ||
		$1 == module "/internal/testutil" { next }
		{
			for (i = 2; i <= NF; i++) {
				if ($i == module "/internal/app" || $i == module "/internal/web") {
					print $1 " imports upper layer " $i
				}
			}
		}' "$tmp"
)"

if [ -n "$violations" ]; then
	printf '%s\n' "$violations" >&2
	exit 1
fi
