#!/usr/bin/env python3
"""Validate a reviewed 0.2 prerelease and its locally built release artifacts.

This tool never writes Git refs, creates releases, or changes the stable manifest.
"""

import argparse
import hashlib
import json
import platform
import re
import subprocess
import tarfile
import tempfile
from pathlib import Path


VERSION = re.compile(r"0\.2\.0-(alpha|rc)\.([1-9][0-9]*)\Z")
SHA = re.compile(r"[0-9a-f]{40}\Z")
ARCHIVES = {
    f"meldra_{os}_{arch}.tar.gz"
    for os in ("Darwin", "Linux")
    for arch in ("amd64", "arm64")
}


def check_progression(version, tags, manifest):
    match = VERSION.fullmatch(version)
    if not match:
        raise ValueError("version must be 0.2.0-alpha.N or 0.2.0-rc.N, with N >= 1")
    if not re.fullmatch(r"0\.1\.[0-9]+", manifest):
        raise ValueError("stable manifest must still identify the released 0.1.x version")
    stable_tags = [tuple(map(int, tag[1:].split("."))) for tag in tags
                   if re.fullmatch(r"v[0-9]+\.[0-9]+\.[0-9]+", tag)]
    if any(tag >= (0, 2, 0) for tag in stable_tags):
        raise ValueError("0.2.0 is already tagged; its prerelease channel is closed")
    if f"v{version}" in tags:
        raise ValueError("candidate tag already exists; do not replace published artifacts")
    existing = {"alpha": [], "rc": []}
    for tag in tags:
        previous = VERSION.fullmatch(tag.removeprefix("v"))
        if previous and tag.startswith("v"):
            existing[previous[1]].append(int(previous[2]))
    channel, number = match[1], int(match[2])
    if channel == "alpha" and existing["rc"]:
        raise ValueError("cannot return to alpha after a release candidate")
    if channel == "rc" and not existing["alpha"]:
        raise ValueError("publish and evaluate an alpha before the first release candidate")
    expected = max(existing[channel], default=0) + 1
    if number != expected:
        raise ValueError(f"next {channel} version must be 0.2.0-{channel}.{expected}")


def git(*args):
    return subprocess.check_output(["git", *args], text=True).strip()


def check_build_info(build, name, version, commit):
    settings = {}
    module_version = None
    for line in build.splitlines():
        match = re.fullmatch(r"\s*build\s+(\S+)=(.*)", line)
        if match:
            settings[match[1]] = match[2]
        fields = line.split()
        if len(fields) >= 3 and fields[:2] == ["mod", "meldra"]:
            module_version = fields[2]
    os, arch = name.removeprefix("meldra_").removesuffix(".tar.gz").split("_")
    expected = {"vcs.revision": commit, "vcs.modified": "false", "CGO_ENABLED": "0",
                "GOOS": os.lower(), "GOARCH": arch}
    if any(settings.get(key) != value for key, value in expected.items()):
        raise ValueError(f"binary source identity or target differs: {name}")
    # Go omits linker flags from build info with -trimpath. The tagged module
    # version is independently checked here; native smoke jobs check --version.
    if module_version != f"v{version}":
        raise ValueError(f"binary module version differs: {name}")


def check_candidate(version, commit):
    if not SHA.fullmatch(commit):
        raise ValueError("commit must be a full lowercase 40-character Git SHA")
    manifest = json.loads(Path(".release-please-manifest.json").read_text())["."]
    check_progression(version, git("tag", "--list").splitlines(), manifest)
    if git("rev-parse", "HEAD") != commit:
        raise ValueError("checked-out HEAD does not match the reviewed commit")
    if git("status", "--porcelain", "--untracked-files=normal"):
        raise ValueError("release source must have a clean working tree")
    notes = Path("docs/release-0.2.0.md")
    if not notes.is_file():
        raise ValueError("release notes are missing from the reviewed commit")


def check_artifacts(dist, version, commit, smoke=False):
    if not VERSION.fullmatch(version) or not SHA.fullmatch(commit):
        raise ValueError("invalid prerelease version or commit")
    metadata = json.loads((dist / "metadata.json").read_text())
    for key, expected in {"version": version, "tag": f"v{version}", "commit": commit}.items():
        if metadata.get(key) != expected:
            raise ValueError(f"artifact metadata {key} differs from reviewed source")
    actual = {file.name for file in dist.glob("*.tar.gz")}
    if actual != ARCHIVES:
        raise ValueError(f"unexpected archive set: {sorted(actual)}")
    checksums = {}
    for line in (dist / "meldra_checksums.txt").read_text().splitlines():
        match = re.fullmatch(r"([0-9a-f]{64})  ([A-Za-z0-9_.-]+)", line)
        if not match or match[2] in checksums:
            raise ValueError("malformed or duplicate checksum entry")
        checksums[match[2]] = match[1]
    if set(checksums) != ARCHIVES:
        raise ValueError("checksums must cover exactly the four release archives")
    for name, digest in checksums.items():
        if hashlib.sha256((dist / name).read_bytes()).hexdigest() != digest:
            raise ValueError(f"checksum mismatch: {name}")
    host_arch = {"x86_64": "amd64", "aarch64": "arm64", "arm64": "arm64"}.get(platform.machine())
    host_archive = f"meldra_{platform.system()}_{host_arch}.tar.gz"
    if smoke and host_archive not in ARCHIVES:
        raise ValueError("native smoke test requires a supported release platform")
    for name in sorted(ARCHIVES):
        with tarfile.open(dist / name, "r:gz") as archive, tempfile.TemporaryDirectory() as directory:
            members = archive.getmembers()
            if len({m.name for m in members}) != len(members):
                raise ValueError(f"duplicate archive member: {name}")
            by_name = {member.name: member for member in members}
            if set(by_name) != {"meldra", "LICENSE", "README.md", "CHANGELOG.md"}:
                raise ValueError(f"unexpected archive members: {name}")
            if any(not member.isfile() for member in members):
                raise ValueError(f"non-regular archive member: {name}")
            binary = Path(directory) / "meldra"
            binary.write_bytes(archive.extractfile(by_name["meldra"]).read())
            build = subprocess.check_output(["go", "version", "-m", str(binary)], text=True)
            check_build_info(build, name, version, commit)
            if smoke and name == host_archive:
                binary.chmod(0o700)
                observed = subprocess.check_output([str(binary), "--version"], text=True).strip()
                if observed != version:
                    raise ValueError(f"binary --version differs: {observed}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    subparsers = parser.add_subparsers(dest="command", required=True)
    for command in ("candidate", "artifacts"):
        child = subparsers.add_parser(command)
        child.add_argument("--version", required=True)
        child.add_argument("--commit", required=True)
        if command == "artifacts":
            child.add_argument("--dist", type=Path, default=Path("dist"))
            child.add_argument("--smoke", action="store_true")
    args = parser.parse_args()
    try:
        if args.command == "candidate":
            check_candidate(args.version, args.commit)
        else:
            check_artifacts(args.dist, args.version, args.commit, args.smoke)
    except (ValueError, KeyError, OSError, subprocess.CalledProcessError, tarfile.TarError) as error:
        parser.exit(1, f"release validation: {error}\n")
    print(f"validated {args.version} at {args.commit}")


if __name__ == "__main__":
    main()
