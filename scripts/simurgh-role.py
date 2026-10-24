#!/usr/bin/env python3
"""Choose Simurgh's server role using the upstream installer's country rules."""
from __future__ import annotations

import argparse
import json
import os
import urllib.error
import urllib.request
from pathlib import Path
from typing import Optional

COUNTRY_URLS = (
    "https://ipapi.co/country/",
    "https://ifconfig.co/country-iso",
    "https://ipinfo.io/country",
)
VALID_ROLES = {"exit", "relay"}


def detect_country() -> str:
    override = os.environ.get("SIMURGH_COUNTRY", "").strip().upper()
    if len(override) == 2 and override.isalpha():
        return override
    for url in COUNTRY_URLS:
        try:
            request = urllib.request.Request(url, headers={"User-Agent": "Simurgh-Role-Detection/1"})
            with urllib.request.urlopen(request, timeout=6) as response:
                country = response.read(32).decode("ascii", "ignore").strip().upper()
            if len(country) == 2 and country.isalpha():
                return country
        except (OSError, TimeoutError, urllib.error.URLError):
            continue
    return ""


def is_unpaired_phoenix_relay(path: Path) -> bool:
    """Recognize the old PHOENIX-created idle relay so foreign hosts can migrate."""
    if not path.is_file():
        return False
    try:
        try:
            import tomllib
        except ImportError:  # Python 3.9/3.10 installations supported by Simurgh
            import tomli as tomllib
        data = tomllib.loads(path.read_text(encoding="utf-8"))
    except (ImportError, OSError, ValueError):
        return False
    exit_config = data.get("exit") or {}
    return (
        data.get("unpaired") is True
        and not data.get("mapping")
        and not exit_config.get("address")
    )


def choose_role(home: Path, country: Optional[str] = None, override: Optional[str] = None) -> dict:
    explicit = (override or "").strip().lower()
    if explicit:
        if explicit not in VALID_ROLES:
            raise ValueError("PHOENIX_SIMURGH_ROLE must be 'exit' or 'relay'")
        return {"role": explicit, "country": country or "", "migrate_relay": False}

    marker = home / "phoenix-role"
    saved = marker.read_text(encoding="utf-8").strip().lower() if marker.is_file() else ""
    if saved not in VALID_ROLES:
        saved = ""

    exit_exists = (home / "exit.toml").is_file()
    relay_path = home / "relay.toml"
    relay_exists = relay_path.is_file()
    if exit_exists and not relay_exists:
        return {"role": "exit", "country": country or "", "migrate_relay": False}
    if relay_exists and not exit_exists:
        detected = country if country is not None else detect_country()
        # A previous PHOENIX run may have persisted relay after a bad geo-IP
        # response. Re-check country for the exact legacy, unpaired config and
        # allow migration on a foreign host even when that stale marker exists.
        if detected and detected != "IR" and is_unpaired_phoenix_relay(relay_path):
            return {"role": "exit", "country": detected, "migrate_relay": True}
        return {"role": "relay", "country": detected or "", "migrate_relay": False}
    if exit_exists and relay_exists:
        # Preserve a deliberately configured dual-role home only when PHOENIX
        # has a marker; without one keep the upstream CLI's relay default.
        return {"role": saved or "relay", "country": country or "", "migrate_relay": False}
    if saved:
        return {"role": saved, "country": country or "", "migrate_relay": False}

    detected = country if country is not None else detect_country()
    # Match the upstream non-interactive installer: Iran is relay; any other
    # known country is exit; when geo lookup is unavailable, default to exit.
    role = "relay" if detected == "IR" else "exit"
    return {"role": role, "country": detected or "", "migrate_relay": False}


def main(argv: Optional[list[str]] = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--home", type=Path, default=Path(os.environ.get("SIMURGH_HOME", "/etc/phoenix-simurgh")))
    parser.add_argument("--json", action="store_true")
    args = parser.parse_args(argv)
    try:
        result = choose_role(
            args.home,
            override=os.environ.get("PHOENIX_SIMURGH_ROLE"),
        )
    except ValueError as exc:
        parser.error(str(exc))
    if args.json:
        print(json.dumps(result, separators=(",", ":")))
    else:
        print(result["role"])
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
