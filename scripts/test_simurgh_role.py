#!/usr/bin/env python3
"""Offline tests for upstream-compatible Simurgh server-role selection."""
from __future__ import annotations

import importlib.util
import json
import os
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

SCRIPT = Path(__file__).with_name("simurgh-role.py")
SPEC = importlib.util.spec_from_file_location("simurgh_role", SCRIPT)
assert SPEC and SPEC.loader
role_module = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(role_module)


class SimurghRoleTests(unittest.TestCase):
    def test_new_install_uses_country_and_safe_upstream_fallback(self):
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            self.assertEqual(role_module.choose_role(home, country="IR")["role"], "relay")
            self.assertEqual(role_module.choose_role(home, country="DE")["role"], "exit")
            self.assertEqual(role_module.choose_role(home, country="")["role"], "exit")

    def test_existing_configs_are_preserved_except_old_idle_foreign_relay(self):
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            relay = home / "relay.toml"
            relay.write_text('unpaired = true\nmapping = []\n[exit]\naddress = ""\n', encoding="utf-8")
            result = role_module.choose_role(home, country="DE")
            self.assertEqual(result, {"role": "exit", "country": "DE", "migrate_relay": True})
            self.assertEqual(role_module.choose_role(home, country="IR")["role"], "relay")

            # An earlier mistaken role marker must not pin an untouched legacy
            # relay on a foreign host after country detection becomes available.
            (home / "phoenix-role").write_text("relay\n", encoding="utf-8")
            self.assertEqual(role_module.choose_role(home, country="DE"), result)

            relay.write_text('unpaired = false\nmapping = []\n', encoding="utf-8")
            self.assertEqual(role_module.choose_role(home, country="DE")["role"], "relay")

    def test_existing_exit_config_wins_and_marker_selects_a_dual_role_home(self):
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            (home / "exit.toml").write_text("token = 'keep'\n", encoding="utf-8")
            self.assertEqual(role_module.choose_role(home, country="IR")["role"], "exit")
            (home / "phoenix-role").write_text("relay\n", encoding="utf-8")
            self.assertEqual(role_module.choose_role(home, country="DE")["role"], "exit")
            (home / "relay.toml").write_text("mapping = [{listen = 1234}]\n", encoding="utf-8")
            self.assertEqual(role_module.choose_role(home, country="DE")["role"], "relay")

    def test_explicit_role_override_is_validated(self):
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            self.assertEqual(role_module.choose_role(home, country="DE", override="relay")["role"], "relay")
            with self.assertRaises(ValueError):
                role_module.choose_role(home, country="IR", override="unknown")

    def test_country_detection_tries_upstream_endpoints_in_order(self):
        responses = iter([OSError("offline"), b" de\n"])

        class Response:
            def __init__(self, body):
                self.body = body

            def __enter__(self):
                return self

            def __exit__(self, *_args):
                return None

            def read(self, _limit):
                return self.body

        def fake_open(request, timeout):
            self.assertEqual(timeout, 6)
            self.assertTrue(request.full_url.startswith("https://"))
            value = next(responses)
            if isinstance(value, Exception):
                raise value
            return Response(value)

        with patch.dict(os.environ, {"SIMURGH_COUNTRY": ""}), patch.object(
            role_module.urllib.request, "urlopen", side_effect=fake_open
        ) as urlopen:
            self.assertEqual(role_module.detect_country(), "DE")
            self.assertEqual(urlopen.call_count, 2)

    def test_cli_reports_machine_readable_role(self):
        with tempfile.TemporaryDirectory() as temporary, patch.dict(os.environ, {"SIMURGH_COUNTRY": "DE"}):
            output = role_module.main(["--home", temporary, "--json"])
            self.assertEqual(output, 0)


if __name__ == "__main__":
    unittest.main()
