#!/usr/bin/env python3
"""Exercise the repository check against real temporary Git indexes."""

from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

CHECK = Path(__file__).with_name("check-repository.py").resolve()


class RepositoryCheckTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="repository-check-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.git("init", "-q")
        self.git("config", "user.name", "Repository test")
        self.git("config", "user.email", "repository@example.test")

    def git(self, *args):
        return subprocess.check_output(["git", *args], cwd=self.root, stderr=subprocess.PIPE)

    def write(self, name, data):
        path = self.root / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(data)

    def check(self, *args):
        return subprocess.run([sys.executable, str(CHECK), *args], cwd=self.root, capture_output=True, text=True)

    def test_product_docs_tests_and_synthetic_fixtures_are_allowed(self):
        self.write("README.md", "# Product guide\n")
        self.write("AGENTS.md", "Read DEVELOPMENT.md\n")
        self.write("backend/example_test.go", "package example\n")
        self.write("backend/testdata/identity.json", '{"email":"person@example.test","ip":"192.0.2.1"}')
        self.write("backend/responses_compaction_prompt.txt", "A required embedded product resource.")
        self.git("add", ".")
        self.assertEqual(self.check("--staged").returncode, 0)

    def test_force_added_records_are_rejected(self):
        self.write(".gitignore", "/architecture/\n")
        self.write("architecture/iterations/local/report.md", "private process record")
        self.git("add", ".gitignore")
        self.git("add", "-f", "architecture")
        result = self.check("--staged")
        self.assertEqual(result.returncode, 1)
        self.assertIn("private records", result.stdout)
        self.assertNotIn("private process record", result.stdout)

    def test_staged_secret_cannot_be_hidden_by_clean_working_copy(self):
        secret = "gh" + "p_" + "0123456789abcdef" * 3
        self.write("backend/config.go", 'const token = "' + secret + '"\n')
        self.git("add", ".")
        self.write("backend/config.go", "package example\n")
        self.assertEqual(self.check().returncode, 0)
        result = self.check("--staged")
        self.assertEqual(result.returncode, 1)
        self.assertIn("GitHub token", result.stdout)
        self.assertNotIn(secret, result.stdout + result.stderr)

    def test_new_working_files_and_raw_payloads_are_checked(self):
        self.write("backend/testdata/capture.sse", "private conversation")
        result = self.check()
        self.assertEqual(result.returncode, 1)
        self.assertNotIn("private conversation", result.stdout + result.stderr)

    def test_assignments_distinguish_declared_examples_from_secret_shapes(self):
        self.write("backend/example_test.go", 'const jwtSecret = "' + ("1234567890" * 4)[:32] + '"')
        self.assertEqual(self.check().returncode, 0)
        value = "aB9dE2gH5jK8mN1pQ4sT7vW0yZ3cF6iL"
        self.write("backend/settings.go", 'const jwtSecret = "' + value + '"')
        result = self.check()
        self.assertEqual(result.returncode, 1)
        self.assertIn("credential-like assignment", result.stdout)
        self.assertNotIn(value, result.stdout + result.stderr)

    def test_private_key_is_rejected_even_in_test_source(self):
        marker = "-----BEGIN " + "PRIVATE KEY-----"
        self.write("backend/example_test.go", marker + "\nprivate bytes")
        self.git("add", ".")
        result = self.check("--staged")
        self.assertEqual(result.returncode, 1)
        self.assertIn("private key", result.stdout)
        self.assertNotIn("private bytes", result.stdout + result.stderr)

    def test_staged_deletion_removes_record_from_candidate(self):
        self.write("notes.log", "old local output")
        self.git("add", ".")
        self.git("commit", "-qm", "test fixture")
        self.git("rm", "notes.log")
        self.write("README.md", "# Source\n")
        self.git("add", ".")
        self.assertEqual(self.check("--staged").returncode, 0)
        self.assertEqual(self.check("--tree", "HEAD").returncode, 1)

    def test_size_and_symlink_cannot_bypass_policy(self):
        self.write("fixture.bin", "x" * (2 * 1024 * 1024 + 1))
        (self.root / "linked-source").symlink_to("fixture.bin")
        self.git("add", ".")
        result = self.check("--staged")
        self.assertEqual(result.returncode, 1)
        self.assertIn("2 MiB", result.stdout)
        self.assertIn("symlinks", result.stdout)


if __name__ == "__main__":
    unittest.main()
