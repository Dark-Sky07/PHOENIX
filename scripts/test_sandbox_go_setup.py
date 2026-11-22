#!/usr/bin/env python3
"""Offline checks for the sandbox-only dependency cache builder."""
import importlib.util
import io
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
import zipfile

spec = importlib.util.spec_from_file_location("sandbox_go_setup", Path(__file__).with_name("setup-sandbox-go.py"))
setup = importlib.util.module_from_spec(spec)
spec.loader.exec_module(setup)


class ProxyTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.proxy = Path(self.temp.name)

    @staticmethod
    def source_zip(files):
        result = io.BytesIO()
        with zipfile.ZipFile(result, "w") as archive:
            for name, data in files.items():
                archive.writestr("example-release/" + name, data)
        return result.getvalue()

    def test_accepts_only_pinned_module_and_excludes_nested_module(self):
        path, version = "github.com/test/example", "v1.0.0"
        mod, code = b"module github.com/test/example\n", b"package example\n"
        files = {path + "@" + version + "/go.mod": mod, path + "@" + version + "/example.go": code}
        src = self.source_zip({"go.mod": mod, "example.go": code, "nested/go.mod": b"module other\n", "nested/x.go": b"package other\n"})
        sums = {"mod": setup.go_hash({"go.mod": mod}), "zip": setup.go_hash(files)}
        with patch.object(setup, "PROXY", self.proxy), patch.object(setup, "fetch", return_value=src):
            self.assertIsNone(setup.populate(((path, version), sums)))
        target = self.proxy / path / "@v" / (version + ".zip")
        with zipfile.ZipFile(target) as archive:
            self.assertEqual(sorted(archive.namelist()), sorted(files))

    def test_rejects_bad_checksum_without_writing_proxy(self):
        path, version = "github.com/test/example", "v1.0.0"
        mod = b"module github.com/test/example\n"
        src = self.source_zip({"go.mod": mod, "example.go": b"tampered"})
        sums = {"mod": setup.go_hash({"go.mod": mod}), "zip": "h1:untrusted"}
        with patch.object(setup, "PROXY", self.proxy), patch.object(setup, "fetch", return_value=src):
            error = setup.populate(((path, version), sums))
        self.assertIn("module checksum mismatch", error)
        self.assertFalse((self.proxy / path / "@v" / (version + ".zip")).exists())
        self.assertFalse((self.proxy / path / "@v" / (version + ".mod")).exists())


if __name__ == "__main__":
    unittest.main()
