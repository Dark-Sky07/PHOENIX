# Disposable sandbox Go setup

For a Linux amd64 development sandbox that can reach PyPI and GitHub but not
`proxy.golang.org`, run:

```sh
python3 scripts/setup-sandbox-go.py
source .cache/sandbox-go/env.sh
mkdir -p internal/web/dist && touch internal/web/dist/.gitkeep
go test ./...
```

The helper installs the third-party `go-bin` PyPI wheel for Go 1.27.4 after
checking a pinned SHA-256. This wheel is not a release toolchain or a substitute
for independently verifying an official Go distribution. It builds a local
file proxy from GitHub source archives for versions in the checked-in `go.sum`
and the authenticated pinned Xray module's `go.sum`; reconstructed files with a
checksum mismatch are refused. The environment disables the online checksum
database because it is unreachable here. Only use this proxy for already-pinned
modules with known sums, and review any missing or mismatched modules rather
than bypassing verification. Not every module, platform or architecture is
supported; warnings can be expected for modules not needed on Linux.

The runtime, cache and test artifacts are under ignored `.cache/` and are not
release inputs. They may disappear when the sandbox is rebuilt; rerun the setup
script then. The synthetic `internal/web/dist/.gitkeep` only satisfies Go's
embed requirement during tests and is not a frontend build. Remove it after
testing if it was not already present.
