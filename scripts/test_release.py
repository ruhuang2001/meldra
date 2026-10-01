import hashlib
import io
import json
import subprocess
import tarfile
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import release


COMMIT = "a" * 40
VERSION = "0.2.0-alpha.1"


def build_info(os="linux", arch="amd64", version=VERSION, commit=COMMIT):
    return (f'binary: go1.26.6\n\tmod\tmeldra\tv{version}\n'
            f"\tbuild\tGOOS={os}\n\tbuild\tGOARCH={arch}\n\tbuild\tCGO_ENABLED=0\n"
            f"\tbuild\tvcs.revision={commit}\n\tbuild\tvcs.modified=false\n")


class ProgressionTests(unittest.TestCase):
    def test_first_alpha_preserves_stable_manifest(self):
        release.check_progression(VERSION, ["v0.1.0"], "0.1.0")
        release.check_progression(VERSION, ["v0.1.1"], "0.1.1")

    def test_alpha_sequence(self):
        release.check_progression("0.2.0-alpha.2", ["v0.2.0-alpha.1"], "0.1.0")

    def test_rc_sequence(self):
        tags = ["v0.2.0-alpha.1", "v0.2.0-alpha.2"]
        release.check_progression("0.2.0-rc.1", tags, "0.1.0")
        release.check_progression("0.2.0-rc.2", tags + ["v0.2.0-rc.1"], "0.1.0")

    def test_reject_invalid_versions(self):
        for version in ["0.2.0", "v0.2.0-alpha.1", "0.2.0-alpha.0", "0.2.0-alpha.01",
                        "0.2.0-beta.1", "0.3.0-alpha.1", VERSION + "\n", "$(id)"]:
            with self.subTest(version=version), self.assertRaises(ValueError):
                release.check_progression(version, [], "0.1.0")

    def test_reject_replacement_skipping_and_backwards_channels(self):
        for version, tags in [(VERSION, ["v" + VERSION]), ("0.2.0-alpha.3", ["v" + VERSION]),
                              ("0.2.0-rc.1", []), (VERSION, ["v0.2.0-rc.1"]),
                              (VERSION, ["v0.2.0"]), (VERSION, ["v0.3.0"])]:
            with self.subTest(version=version, tags=tags), self.assertRaises(ValueError):
                release.check_progression(version, tags, "0.1.0")

    def test_reject_pretend_manifest_release(self):
        for manifest in ["0.2.0", VERSION, "garbage"]:
            with self.subTest(manifest=manifest), self.assertRaises(ValueError):
                release.check_progression(VERSION, [], manifest)


class CandidateTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        subprocess.run(["git", "init", "-q", str(self.root)], check=True)
        (self.root / "docs").mkdir()
        (self.root / "docs/release-0.2.0.md").write_text("Reviewed release notes\n")
        (self.root / ".release-please-manifest.json").write_text('{".": "0.1.0"}')
        subprocess.run(["git", "-C", str(self.root), "add", "."], check=True)
        subprocess.run(["git", "-C", str(self.root), "-c", "user.name=Test",
                        "-c", "user.email=test@example.invalid", "commit", "-qm", "fixture"], check=True)
        self.commit = subprocess.check_output(["git", "-C", str(self.root), "rev-parse", "HEAD"], text=True).strip()

    def validate(self, commit=None):
        # Running the CLI in a real checkout exercises git identity and dirty-source checks.
        return subprocess.run(["python3", "-B", str(Path(release.__file__).resolve()), "candidate",
                               "--version", VERSION, "--commit", commit or self.commit],
                              cwd=self.root, capture_output=True, text=True)

    def test_clean_reviewed_source(self):
        result = self.validate()
        self.assertEqual(result.returncode, 0, result.stderr)

    def test_wrong_commit_and_ref_expression(self):
        for commit in [COMMIT, "HEAD", "a" * 39, "a" * 40 + "\n"]:
            with self.subTest(commit=commit):
                self.assertNotEqual(self.validate(commit).returncode, 0)

    def test_dirty_source(self):
        (self.root / "docs/release-0.2.0.md").write_text("changed")
        self.assertIn("clean working tree", self.validate().stderr)

    def test_existing_tag(self):
        subprocess.run(["git", "-C", str(self.root), "tag", "v" + VERSION], check=True)
        self.assertIn("already exists", self.validate().stderr)


class ArtifactTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.dist = Path(self.directory.name)
        self.metadata = {"version": VERSION, "tag": "v" + VERSION, "commit": COMMIT}
        self.write_metadata()
        for name in release.ARCHIVES:
            self.write_archive(name)
        self.write_checksums()

    def write_metadata(self):
        (self.dist / "metadata.json").write_text(json.dumps(self.metadata))

    def write_archive(self, name, extra=None):
        with tarfile.open(self.dist / name, "w:gz") as archive:
            os, arch = name.removeprefix("meldra_").removesuffix(".tar.gz").split("_")
            for member in ["meldra", "LICENSE", "README.md", "CHANGELOG.md"] + ([extra] if extra else []):
                data = f"{os.lower()}/{arch}".encode() if member == "meldra" else b"fixture"
                entry = tarfile.TarInfo(member)
                entry.size = len(data)
                archive.addfile(entry, io.BytesIO(data))

    def write_checksums(self):
        (self.dist / "meldra_checksums.txt").write_text("".join(
            f"{hashlib.sha256((self.dist / name).read_bytes()).hexdigest()}  {name}\n"
            for name in sorted(release.ARCHIVES)))

    def verify(self, output=None):
        def inspect_binary(args, **kwargs):
            os, arch = Path(args[-1]).read_text().split("/")
            return output or build_info(os, arch)
        with patch.object(release.subprocess, "check_output", side_effect=inspect_binary):
            release.check_artifacts(self.dist, VERSION, COMMIT)

    def test_all_archives_and_embedded_source_identity(self):
        self.verify()

    def test_reject_metadata_mismatch(self):
        for key in self.metadata:
            with self.subTest(key=key), self.assertRaises(ValueError):
                metadata = dict(self.metadata)
                self.metadata[key] = "different"
                self.write_metadata()
                self.metadata = metadata
                self.verify()

    def test_reject_missing_or_extra_archive(self):
        (self.dist / "extra.tar.gz").touch()
        with self.assertRaisesRegex(ValueError, "archive set"):
            self.verify()

    def test_reject_corrupted_archive(self):
        (self.dist / sorted(release.ARCHIVES)[0]).write_bytes(b"corrupted")
        with self.assertRaisesRegex(ValueError, "checksum mismatch"):
            self.verify()

    def test_reject_missing_or_duplicate_checksums(self):
        path = self.dist / "meldra_checksums.txt"
        original = path.read_text()
        for content in ["", original + original.splitlines()[0] + "\n"]:
            path.write_text(content)
            with self.subTest(content=content), self.assertRaises(ValueError):
                self.verify()

    def test_reject_unexpected_and_duplicate_archive_members(self):
        for member in ["../../escape", "meldra"]:
            self.write_archive(sorted(release.ARCHIVES)[0], extra=member)
            self.write_checksums()
            with self.subTest(member=member), self.assertRaises(ValueError):
                self.verify()

    def test_reject_wrong_target_version_source_and_dirty_binary(self):
        for output in [build_info(commit="b" * 40), build_info(version=VERSION + "0"),
                       build_info().replace("modified=false", "modified=true"),
                       build_info(os="windows"), build_info().replace("CGO_ENABLED=0", "CGO_ENABLED=1")]:
            with self.subTest(output=output), self.assertRaises(ValueError):
                self.verify(output)


if __name__ == "__main__":
    unittest.main()
