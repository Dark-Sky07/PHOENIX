#!/usr/bin/env python3
"""Rebuild a disposable Linux/amd64 Go toolchain and offline dependency proxy.

For sandboxes that can reach PyPI and GitHub but not proxy.golang.org. This
is a development helper, not a release build or a source of trusted binaries.
The third-party go-bin wheel is pinned by SHA-256; reconstructed module files
are accepted only when their Go hashes match an existing go.sum entry.
"""
import base64
import concurrent.futures
import datetime
import hashlib
import io
import json
import os
from pathlib import Path
import platform
import re
import subprocess
import sys
import urllib.request
import zipfile

ROOT = Path(__file__).resolve().parent.parent
CACHE = ROOT / ".cache" / "sandbox-go"
PROXY = CACHE / "proxy"
GO = CACHE / "runtime" / "go" / "bin" / "go"
WHEEL_URL = "https://files.pythonhosted.org/packages/0b/9b/9f2a265ccbc9b879c87ee944011f414e8192fe32b1e5b6650a08eb1f8134/go_bin-1.27.4-py3-none-manylinux_2_17_x86_64.whl"
WHEEL_HASH = "d7e327a50c4f3ad8605420f0dbe7d20e7ad011de480d1c51804e207ae369ddf2"

# Vanity modules used in the checked-in go.sum and pinned Xray go.sum.
ALIASES = {
    "go.uber.org/atomic": ("uber-go", "atomic", ""),
    "go.uber.org/mock": ("uber-go", "mock", ""),
    "google.golang.org/grpc": ("grpc", "grpc-go", ""),
    "google.golang.org/protobuf": ("protocolbuffers", "protobuf-go", ""),
    "google.golang.org/genproto/googleapis/rpc": ("googleapis", "go-genproto", "googleapis/rpc"),
    "gopkg.in/natefinch/lumberjack.v2": ("natefinch", "lumberjack", ""),
    "gopkg.in/yaml.v3": ("go-yaml", "yaml", ""),
    "gopkg.in/yaml.v2": ("go-yaml", "yaml", ""),
    "gopkg.in/check.v1": ("go-check", "check", ""),
    "go.yaml.in/yaml/v3": ("yaml", "go-yaml", ""),
    "gorm.io/gorm": ("go-gorm", "gorm", ""),
    "gorm.io/driver/postgres": ("go-gorm", "postgres", ""),
    "gorm.io/driver/sqlite": ("go-gorm", "sqlite", ""),
    "go.mongodb.org/mongo-driver/v2": ("mongodb", "mongo-go-driver", ""),
    "go4.org/netipx": ("go4org", "netipx", ""),
    "gvisor.dev/gvisor": ("google", "gvisor", ""),
    "gonum.org/v1/gonum": ("gonum", "gonum", ""),
    "h12.io/socks": ("h12w", "socks", ""),
    "lukechampine.com/blake3": ("lukechampine", "blake3", ""),
    "golang.zx2c4.com/wireguard": ("WireGuard", "wireguard-go", ""),
    "golang.zx2c4.com/wintun": ("trevor403", "wintun-go", ""),
    "golang.zx2c4.com/wireguard/windows": ("WireGuard", "wireguard-windows", ""),
    "go.opentelemetry.io/auto/sdk": ("open-telemetry", "opentelemetry-go-instrumentation", "sdk"),
    "go.opentelemetry.io/otel": ("open-telemetry", "opentelemetry-go", ""),
    "go.opentelemetry.io/otel/metric": ("open-telemetry", "opentelemetry-go", "metric"),
    "go.opentelemetry.io/otel/sdk": ("open-telemetry", "opentelemetry-go", "sdk"),
    "go.opentelemetry.io/otel/sdk/metric": ("open-telemetry", "opentelemetry-go", "sdk/metric"),
    "go.opentelemetry.io/otel/trace": ("open-telemetry", "opentelemetry-go", "trace"),
}


def fetch(url):
    with urllib.request.urlopen(url, timeout=90) as response:
        return response.read()


def go_hash(files):
    lines = "".join(hashlib.sha256(data).hexdigest() + "  " + name + "\n" for name, data in sorted(files.items()))
    return "h1:" + base64.b64encode(hashlib.sha256(lines.encode()).digest()).decode()


def escaped(path):
    return "".join("!" + c.lower() if c.isupper() else c for c in path)


def source(path):
    if path.startswith("github.com/"):
        parts = path.split("/")
        sub = "/".join(parts[3:])
        if re.fullmatch(r"v[0-9]+", sub) and path != "github.com/go-ldap/ldap/v3":
            sub = ""
        return parts[1], parts[2], sub
    if path.startswith("golang.org/x/"):
        return "golang", path.split("/")[2], ""
    return ALIASES.get(path)


def load_sums(data, sums):
    for line in data.splitlines():
        parts = line.split()
        if len(parts) == 3:
            path, version, digest = parts
            mod = version.endswith("/go.mod")
            version = version.removesuffix("/go.mod")
            key = (path, version)
            record = sums.setdefault(key, {})
            kind = "mod" if mod else "zip"
            if kind in record and record[kind] != digest:
                raise ValueError(f"conflicting checksums for {path}@{version}")
            record[kind] = digest


def populate(item):
    (path, version), hashes = item
    dest = PROXY / escaped(path) / "@v"
    mod_path, zip_path = dest / (version + ".mod"), dest / (version + ".zip")
    info_path = dest / (version + ".info")
    if ("mod" not in hashes or mod_path.exists()) and ("zip" not in hashes or zip_path.exists()) and info_path.exists():
        return None
    location = source(path)
    if not location:
        return f"no GitHub mapping for {path}@{version}"
    owner, repo, sub = location
    pseudo = re.search(r"-([0-9a-f]{12})$", version)
    refs = [pseudo.group(1)] if pseudo else ([sub + "/" + version, version] if sub else [version])
    data = None
    for ref in refs:
        try:
            data = fetch(f"https://codeload.github.com/{owner}/{repo}/zip/{ref}")
            break
        except (OSError, ValueError):
            continue
    if data is None:
        return f"source unavailable for {path}@{version}"
    try:
        with zipfile.ZipFile(io.BytesIO(data)) as archive:
            names = archive.namelist()
            top = names[0].split("/")[0] + "/"
            # Some repositories keep major-version modules in a vN directory
            # even when their version tag is at repository root. Pick it only
            # when its go.mod matches the existing expected checksum.
            major = path.rsplit("/", 1)[-1]
            if not sub and re.fullmatch(r"v[0-9]+", major) and "mod" in hashes:
                candidate = top + major + "/go.mod"
                if candidate in names and go_hash({"go.mod": archive.read(candidate)}) == hashes["mod"]:
                    sub = major
            prefix = top + (sub + "/" if sub else "")
            names_by_rel = {
                name[len(prefix):]: name for name in names
                if name.startswith(prefix) and not name.endswith("/")
                and (archive.getinfo(name).external_attr >> 16 & 0o170000) != 0o120000
            }
            if not names_by_rel:
                return f"module directory absent for {path}@{version}"
            mod = archive.read(names_by_rel["go.mod"]) if "go.mod" in names_by_rel else ("module " + path + "\n").encode()
            if "mod" in hashes and go_hash({"go.mod": mod}) != hashes["mod"]:
                return f"go.mod checksum mismatch for {path}@{version}"
            nested = [name[:-len("go.mod")] for name in names_by_rel if name.endswith("/go.mod")]
            files = {}
            if "zip" in hashes:
                for rel, name in names_by_rel.items():
                    if rel.startswith("vendor/") or "/vendor/" in rel or any(rel.startswith(n) for n in nested) or any(x in (".git", ".hg", ".svn", ".bzr") for x in rel.split("/")):
                        continue
                    files[path + "@" + version + "/" + rel] = archive.read(name)
                if sub and "LICENSE" not in names_by_rel and top + "LICENSE" in names:
                    files[path + "@" + version + "/LICENSE"] = archive.read(top + "LICENSE")
                if go_hash(files) != hashes["zip"]:
                    return f"module checksum mismatch for {path}@{version}"
            dest.mkdir(parents=True, exist_ok=True)
            if "mod" in hashes:
                mod_path.write_bytes(mod)
            if files:
                with zipfile.ZipFile(zip_path, "w", zipfile.ZIP_DEFLATED) as out:
                    for name, content in files.items():
                        out.writestr(name, content)
            # Pseudo-versions encode their exact commit time. For tags, use
            # GitHub's archive timestamp instead of inventing a timestamp.
            timestamp = re.search(r"-(\d{14})-[0-9a-f]{12}$", version)
            if timestamp:
                moment = datetime.datetime.strptime(timestamp.group(1), "%Y%m%d%H%M%S")
            else:
                moment = datetime.datetime(*archive.getinfo(names[0]).date_time)
            info_path.write_text(json.dumps({"Version": version, "Time": moment.strftime("%Y-%m-%dT%H:%M:%SZ")}))
    except (OSError, ValueError, zipfile.BadZipFile) as exc:
        return f"failed to prepare {path}@{version}: {exc}"
    return None


def main():
    if platform.system() != "Linux" or platform.machine() not in ("x86_64", "amd64"):
        raise SystemExit("This sandbox helper currently supports Linux amd64 only")
    CACHE.mkdir(parents=True, exist_ok=True)
    if not GO.exists():
        wheel = CACHE / WHEEL_URL.rsplit("/", 1)[-1]
        data = fetch(WHEEL_URL)
        if hashlib.sha256(data).hexdigest() != WHEEL_HASH:
            raise SystemExit("go-bin wheel SHA-256 mismatch; refusing installation")
        wheel.write_bytes(data)
        subprocess.run([sys.executable, "-m", "pip", "install", "--no-index", "--no-deps", "--target", str(CACHE / "runtime"), str(wheel)], check=True)
        wheel.unlink()
    subprocess.run([str(GO), "version"], check=True)
    sums = {}
    load_sums((ROOT / "go.sum").read_text(), sums)
    with concurrent.futures.ThreadPoolExecutor(max_workers=8) as pool:
        failures = [message for message in pool.map(populate, sums.items()) if message]
    xray = next((item for item in sums if item[0] == "github.com/xtls/xray-core" and "64fada32b5b9" in item[1]), None)
    if xray:
        archive_path = PROXY / escaped(xray[0]) / "@v" / (xray[1] + ".zip")
        if archive_path.exists():
            with zipfile.ZipFile(archive_path) as archive:
                load_sums(archive.read(xray[0] + "@" + xray[1] + "/go.sum").decode(), sums)
            with concurrent.futures.ThreadPoolExecutor(max_workers=8) as pool:
                failures += [message for message in pool.map(populate, sums.items()) if message]
    for message in sorted(set(failures)):
        print("warning:", message, file=sys.stderr)
    env = CACHE / "env.sh"
    env.write_text(f'export PATH="{GO.parent}:$PATH"\nexport GOTOOLCHAIN=local\nexport GOPATH="{CACHE / "gopath"}"\nexport GOPROXY="file://{PROXY}"\nexport GOSUMDB=off\n')
    print(f"Run: source {env}")
    print("Only already-pinned go.sum checksums are mirrored; missing entries require review.")


if __name__ == "__main__":
    main()
