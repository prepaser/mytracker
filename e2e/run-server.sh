#!/bin/sh
set -eu

test_dir="$(mktemp -d)"
trap 'rm -rf "$test_dir"' EXIT
go run fixture.go "$test_dir"
go build -o "$test_dir/mytracker" ..
"$test_dir/mytracker" -config "$test_dir/config.toml"
