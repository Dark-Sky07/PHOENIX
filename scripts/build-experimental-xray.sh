#!/usr/bin/env bash
# Experimental test-only build. This does not replace release Xray binaries.
set -euo pipefail
if [ "$#" != 1 ]; then echo "usage: $0 ABSOLUTE_OUTPUT_PATH" >&2; exit 2; fi
case "$1" in /*) output="$1";; *) echo 'output path must be absolute' >&2; exit 2;; esac
root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"
version="$(go list -m -f '{{.Version}}' github.com/xtls/xray-core)"
if [ "$version" != 'v1.260327.1-0.20260710210335-64fada32b5b9' ]; then
  echo "unexpected Xray module $version; review experimental patch" >&2; exit 1
fi
go mod download github.com/xtls/xray-core
source_dir="$(go list -m -f '{{.Dir}}' github.com/xtls/xray-core)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
cp -R "$source_dir"/. "$work"/
chmod -R u+w "$work"
cd "$work"
patch --batch --forward --fuzz=0 -p1 < "$root/patches/xray/experimental-vless-disconnect.patch"
patch --batch --forward --fuzz=0 -p1 < "$root/patches/xray/experimental-trojan-disconnect.patch"
patch --batch --forward --fuzz=0 -p1 < "$root/patches/xray/experimental-vmess-disconnect.patch"
patch --batch --forward --fuzz=0 -p1 < "$root/patches/xray/experimental-shadowsocks-disconnect.patch"
# Patch the exact dependency whose user maps are shared with authentication.
sing_version="$(go list -m -f '{{.Version}}' github.com/sagernet/sing-shadowsocks)"
if [ "$sing_version" != 'v0.2.7' ]; then echo "unexpected sing-shadowsocks module $sing_version" >&2; exit 1; fi
go mod download github.com/sagernet/sing-shadowsocks
sing_dir="$(go list -m -f '{{.Dir}}' github.com/sagernet/sing-shadowsocks)"
mkdir "$work/phoenix-sing-shadowsocks"
cp -R "$sing_dir"/. "$work/phoenix-sing-shadowsocks/"
chmod -R u+w "$work/phoenix-sing-shadowsocks"
(cd "$work/phoenix-sing-shadowsocks" && patch --batch --forward --fuzz=0 -p1 < "$root/patches/xray/experimental-sing-ss2022-snapshot.patch")
go mod edit -replace github.com/sagernet/sing-shadowsocks=./phoenix-sing-shadowsocks
patch --batch --forward --fuzz=0 -p1 < "$root/patches/xray/experimental-shadowsocks-2022-disconnect.patch"
if [ "${PHOENIX_XRAY_RUN_TESTS:-0}" = 1 ]; then
  GOWORK=off go test -mod=mod -race ./proxy/vless/inbound ./proxy/trojan ./proxy/vmess/inbound ./proxy/shadowsocks ./proxy/shadowsocks_2022 ./app/proxyman/inbound -run '^TestPhoenix(Vless|Trojan|Vmess|Shadowsocks|SS2022|UDP)' -count=1
fi
if [ "${PHOENIX_XRAY_RACE_BINARY:-0}" = 1 ]; then
  GOWORK=off go build -race -mod=mod -o "$output" ./main
else
  GOWORK=off go build -mod=mod -o "$output" ./main
fi
