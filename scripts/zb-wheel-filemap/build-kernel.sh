#!/usr/bin/env bash
# Build the stock vendor source with tracefs event tracing in a separate tree.
set -euo pipefail
repo=$1
target=$2
stock=$3
commit=7483450dec64b4f923037de31d8c28298a7c3359
test "$(git -C "$repo" rev-parse HEAD)" = "$commit"
echo "99d0950a178b92a1db3d5ee233dc6a320c8892334b99ba6fea11bd7be3745067  $stock" | sha256sum -c -
mkdir -p "$target"
git clone -q --shared --no-checkout "$repo" "$target/src"
git -C "$target/src" checkout -q --detach "$commit"
cp "$stock" "$target/src/.config"
cd "$target/src"
scripts/config --enable FTRACE --enable CACHESTAT_SYSCALL --enable ENABLE_DEFAULT_TRACERS --disable FUNCTION_TRACER \
  --disable FUNCTION_GRAPH_TRACER --disable DYNAMIC_FTRACE --disable KPROBE_EVENTS \
  --disable UPROBE_EVENTS --disable FTRACE_SYSCALLS --disable HIST_TRIGGERS
make olddefconfig
grep -qx 'CONFIG_EVENT_TRACING=y' .config
grep -qx 'CONFIG_TRACEPOINTS=y' .config
grep -qx '# CONFIG_FUNCTION_TRACER is not set' .config
scripts/diffconfig "$stock" .config > "$target/config-delta.txt"
make -j"$(nproc)" LOCALVERSION=-fcmod vmlinux
nm vmlinux > "$target/kernel-symbols"
grep -E ' (__tracepoint_mm_filemap_delete_from_page_cache|event_mm_filemap_delete_from_page_cache)$' "$target/kernel-symbols" > "$target/deletion-symbols.txt"
cp .config "$target/tracing.config"
cp vmlinux "$target/tracing.vmlinux"
(cd "$target" && sha256sum tracing.config tracing.vmlinux > SHA256SUMS)
