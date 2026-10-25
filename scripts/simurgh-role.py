#!/usr/bin/env python3
"""Preserve Simurgh's installed role; require a manual choice for fresh installs."""
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

    # A single geo-IP answer must not label an overseas server as the Iran
    # relay. Keep checking the independent endpoints: two matching results are
    # enough, and any unresolved result falls back to the installer's safe exit
    # default. A lone non-Iran result is also safe because every non-Iran role
    # is exit; a lone Iran result is not strong enough to select relay.
    votes: list[str] = []
    for url in COUNTRY_URLS:
        try:
            request = urllib.request.Request(url, headers={"User-Agent": "Simurgh-Role-Detection/1"})
            with urllib.request.urlopen(request, timeout=6) as response:
                country = response.read(32).decode("ascii", "ignore").strip().upper()
            if len(country) == 2 and country.isalpha():
                votes.append(country)
                if votes.count(country) >= 2:
                    return country
        except (OSError, TimeoutError, urllib.error.URLError):
            continue

    non_iran = next((country for country in votes if country != "IR"), "")
    return non_iran


def is_unpaired_phoenix_relay(path: Path) -> bool:
    """Recognize a relay config with no pairing or user traffic to preserve."""
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

    # Some legacy relay configs may lack an explicit ``unpaired = true`` flag.
    # Recognize that empty shape too, but only when
    # there is no token, exit endpoint, pool, mapping, or reverse-mode setup.
    # Never re-role a relay that has pairing credentials or user traffic rules.
    exit_config = data.get("exit") or {}
    if not isinstance(exit_config, dict):
        return False
    return (
        data.get("dial") != "exit"
        and not data.get("token")
        and not data.get("tokens")
        and not data.get("pool")
        and not data.get("mapping")
        and not data.get("mappings")
        and not exit_config.get("address")
    )


def choose_role(home: Path, country: Optional[str] = None, override: Optional[str] = None) -> dict:
    explicit = (override or "").strip().lower()
    if explicit and explicit not in VALID_ROLES:
        raise ValueError("PHOENIX_SIMURGH_ROLE must be 'exit' or 'relay'")

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
        # Keep the narrowly scoped PHOENIX migration for an idle legacy relay;
        # never use geolocation to choose a role on a fresh installation.
        if detected != "IR" and is_unpaired_phoenix_relay(relay_path):
            return {"role": "exit", "country": detected, "migrate_relay": True}
        return {"role": "relay", "country": detected or "", "migrate_relay": False}
    if exit_exists and relay_exists:
        detected = country if country is not None else detect_country()
        # An interrupted earlier migration can leave the staged exit config
        # beside the old, idle relay config. Complete only that safe migration;
        # preserve intentionally configured dual-role homes and active relays.
        if detected != "IR" and is_unpaired_phoenix_relay(relay_path):
            return {"role": "exit", "country": detected, "migrate_relay": True}
        return {"role": saved or "relay", "country": detected or "", "migrate_relay": False}
    if saved:
        return {"role": saved, "country": country or "", "migrate_relay": False}
    if explicit:
        return {"role": explicit, "country": country or "", "migrate_relay": False}

    # A new server has no reliable stored role. The native installer asks the
    # operator; non-interactive Docker setups must pass PHOENIX_SIMURGH_ROLE.
    return {"role": "", "country": "", "migrate_relay": False}


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
