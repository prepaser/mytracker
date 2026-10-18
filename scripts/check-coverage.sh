#!/bin/sh
set -eu

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

check() {
  name=$1
  profile=$2
  minimum=$3
  actual=$(go tool cover -func="$profile" | awk '/^total:/ {gsub("%", "", $3); print $3}')
  awk -v actual="$actual" -v minimum="$minimum" 'BEGIN { exit actual + 0 >= minimum + 0 ? 0 : 1 }' ||
    { echo "$name coverage ${actual}% is below ${minimum}%" >&2; exit 1; }
  echo "$name coverage: ${actual}%"
}

package_list="$tmp/packages"
go list ./... >"$package_list"
packages=$(awk '!/\/internal\/testutil$/' "$package_list")
cover_packages=$(printf '%s\n' "$packages" | paste -sd, -)
go test -count=1 -short -coverpkg="$cover_packages" -coverprofile="$tmp/all.out" $packages
check overall "$tmp/all.out" 81.5

for spec in \
  "config ./internal/config 80" \
  "store ./internal/store 80" \
  "stamp ./internal/stamp 75" \
  "proofverify ./internal/proofverify 80" \
  "maintenance ./internal/maintenance 85" \
  "import ./internal/importzip 80" \
  "web-auth ./internal/web 80"
do
  set -- $spec
  package=$(go list "$2")
  awk -v prefix="$package/" 'NR == 1 || index($1, prefix) == 1' "$tmp/all.out" >"$tmp/$1.out"
  check "$1" "$tmp/$1.out" "$3"
done
