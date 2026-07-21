#!/bin/sh

set -eu

repository="ruhuang2001/meldra"
install_dir=${INSTALL_DIR:-"$HOME/.local/bin"}
version=${VERSION:-latest}

fail() {
  printf 'meldra installer: %s\n' "$*" >&2
  exit 1
}

for command in curl tar; do
  command -v "$command" >/dev/null 2>&1 || fail "required command not found: $command"
done

if command -v sha256sum >/dev/null 2>&1; then
  checksum_command="sha256sum"
elif command -v shasum >/dev/null 2>&1; then
  checksum_command="shasum -a 256"
else
  fail "required command not found: sha256sum or shasum"
fi

case $(uname -s) in
  Darwin) os=Darwin ;;
  Linux) os=Linux ;;
  *) fail "unsupported operating system: $(uname -s) (supported: macOS and Linux)" ;;
esac

case $(uname -m) in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) fail "unsupported architecture: $(uname -m) (supported: amd64 and arm64)" ;;
esac

archive="meldra_${os}_${arch}.tar.gz"
if [ "$version" = latest ]; then
  download_url="https://github.com/${repository}/releases/latest/download"
else
  download_url="https://github.com/${repository}/releases/download/${version}"
fi

temp_dir=$(mktemp -d "${TMPDIR:-/tmp}/meldra.XXXXXX")
trap 'rm -rf "$temp_dir"' EXIT HUP INT TERM

printf 'Downloading Meldra %s for %s/%s...\n' "$version" "$os" "$arch"
curl -fsSL "${download_url}/${archive}" -o "${temp_dir}/${archive}"
curl -fsSL "${download_url}/meldra_checksums.txt" -o "${temp_dir}/meldra_checksums.txt"

(
  cd "$temp_dir"
  checksum_line=""
  while IFS= read -r line; do
    case "$line" in
      *"  ${archive}") checksum_line=$line; break ;;
    esac
  done < meldra_checksums.txt
  [ -n "$checksum_line" ] || fail "checksum not found for ${archive}"
  printf '%s\n' "$checksum_line" > meldra_checksum.txt
  if [ "$checksum_command" = sha256sum ]; then
    sha256sum -c meldra_checksum.txt
  else
    shasum -a 256 -c meldra_checksum.txt
  fi
)

tar -xzf "${temp_dir}/${archive}" -C "$temp_dir"
[ -f "${temp_dir}/meldra" ] || fail "archive does not contain the meldra binary"

mkdir -p "$install_dir"
cp "${temp_dir}/meldra" "${install_dir}/meldra"
chmod 0755 "${install_dir}/meldra"
printf 'Installed Meldra to %s/meldra\n' "$install_dir"

case ":${PATH:-}:" in
  *":${install_dir}:"*) ;;
  *)
    printf 'Add %s to your PATH, for example:\n  export PATH="%s:$PATH"\n' "$install_dir" "$install_dir"
    ;;
esac
