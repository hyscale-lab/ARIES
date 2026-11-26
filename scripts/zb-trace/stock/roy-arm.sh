#!/usr/bin/env bash
# Run on a staged solo worker, with no other active campaign sandbox.
set -eu -o pipefail
: "${1:?order1, order9, or pack}" "${2:?fresh run label}"
case "$1" in order1|order9|pack) ;; *) exit 2;; esac
[[ "$2" =~ ^[a-zA-Z0-9-]+$ ]]
base=/local/zb-study3-stock
cd "$base"
service="$2"
test ! -e "$service"
sudo -n unshare --mount --net --propagation private /bin/bash "$base/smoke.sh" "$service" serve "$base/$1.toml" >"$service-service.log" 2>&1 &
launcher=$!
finish() {
    if sudo test -s "$service/server.pid"; then
        server=$(sudo cat "$service/server.pid")
        if [[ "$(sudo readlink "/proc/$server/exe")" == "$base/server-bundle/server" ]]; then
            sudo kill -TERM "$server"
        fi
    fi
    wait "$launcher" || true
}
trap finish EXIT
for _ in $(seq 60); do
    sudo test -s "$service/server.pid" && break
    kill -0 "$launcher"
    sleep 1
done
pid=$(sudo cat "$service/server.pid")
ns=(sudo -n nsenter -t "$pid" -n --)
cli="$base/cli-bundle/aenv"
pack_store=
if [[ "$1" == pack ]]; then pack_store="$base/$service/home/snapshot-store/repository"; fi
"${ns[@]}" env STOCK_PACK_STORE="$pack_store" XDG_CONFIG_HOME="$base/$service/xdg" AENV_CLI="$cli" STOCK_BASE_IMAGE=127.0.0.1:18762/library/ubuntu@sha256:9a04f13b3f80ef056361e30868593b1cfd2279fe19e4081cb93a90bf56819d6a \
    "$base/prepare-pilot.sh" roy 127.0.0.1:18762/library/roy@sha256:3ed43bff698735f15fa9db6e020df8e44e1d3ec37e8828ebc53eba054e509a77 "$base/runc" "$base/$service-prepared"
opt=(--deep)
if [[ "$1" == pack ]]; then opt+=(--checkpoint); fi
"${ns[@]}" python3 -B "$base/scripts/zb-trace/stock/run-pilot.py" "$base/$service-prepared" "$base/$service-run" --service "$service" "${opt[@]}"
