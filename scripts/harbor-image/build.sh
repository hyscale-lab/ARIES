#!/usr/bin/env bash
# Run from the ARIES root using Rust 1.95.0 and the musl target.
set -euo pipefail
test "$#" -eq 1
root=$(pwd)
test -e "$root/scripts/harbor-image/Cargo.toml"
rustc --version | grep -q '^rustc 1.95.0 '
cargo_home=$(realpath "${CARGO_HOME:-$HOME/.cargo}")
export RUSTFLAGS="--remap-path-prefix=$root=/workspace --remap-path-prefix=$cargo_home=/cargo -C strip=symbols"
target_dir=$(mktemp -d)
trap 'rm -rf -- "$target_dir"' EXIT
CARGO_TARGET_DIR="$target_dir" cargo build --locked --release --target x86_64-unknown-linux-musl --manifest-path scripts/harbor-image/Cargo.toml
install -m 0755 "$target_dir/x86_64-unknown-linux-musl/release/aries-harbor-image" "$1"
sha256sum "$1"
