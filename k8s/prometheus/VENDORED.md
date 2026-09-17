# Vendored chart provenance

`chart/` is an unmodified copy of the upstream **kube-prometheus-stack** chart.
It is committed rather than fetched so an install needs no chart-repository
access, and so the exact templates a cluster received are recoverable from git.

| | |
|---|---|
| Chart | `kube-prometheus-stack` |
| Version | `72.6.2` (appVersion `v0.82.2`) |
| Source | `https://github.com/prometheus-community/helm-charts/releases/download/kube-prometheus-stack-72.6.2/kube-prometheus-stack-72.6.2.tgz` |
| Repository | `https://prometheus-community.github.io/helm-charts` |
| Tarball sha256 | `cee5bcf2091e688fb6b081f9d119143231d7f4fcd8cc32afb59cc2ae70728cd8` |
| Size | 6.7 MB, 323 files (6 CRD files are 3.8 MB of it) |

The sha256 above is the digest the chart repository's `index.yaml` publishes for
this version, not just the digest of what was downloaded.

Subchart versions are recorded in `chart/Chart.lock`; Grafana is `9.0.0`.

## Do not edit `chart/`

Overrides belong in the values files, which are applied in this order:

1. `chart/values.yaml` — upstream defaults, untouched
2. `values.yaml` — Prometheus-side overrides
3. `../grafana/values.yaml` — Grafana overrides
4. a placement file `aries-setup` renders from `node_role`

Later files win, so placement overrides anything the first three set.

## Refreshing to a new version

```sh
V=<new-version>
curl -fsSLO "https://github.com/prometheus-community/helm-charts/releases/download/kube-prometheus-stack-$V/kube-prometheus-stack-$V.tgz"

# Verify against the digest the repository publishes for that version:
curl -fsSL https://prometheus-community.github.io/helm-charts/index.yaml \
  | grep -B25 "kube-prometheus-stack-$V.tgz" | grep digest:
shasum -a 256 "kube-prometheus-stack-$V.tgz"

rm -rf chart && tar xzf "kube-prometheus-stack-$V.tgz" && mv kube-prometheus-stack chart
```

## macOS: do not copy this chart with a plain `tar`

Every file here carries the `com.apple.provenance` extended attribute, added by
macOS to anything downloaded. `xattr -d` cannot remove it — the call exits 0 and
the attribute stays — so this is a permanent property of the files, not
something to clean up.

macOS `tar` serialises each extended attribute as a companion archive member
named `._<file>`, holding binary resource-fork data. Extracted by GNU tar on a
Linux node they become real files, and because `._` sorts before any letter,
the first thing `helm` parses in `chart/crds/` is a resource fork:

```
Error: failed to install CRD crds/._crd-alertmanagerconfigs.yaml:
  error converting YAML to JSON: yaml: control characters are not allowed
```

`aries-setup` prevents this: its staging pipeline runs tar with
`COPYFILE_DISABLE=1` and `--exclude '._*'`, and `setup_prometheus` refuses to
install a chart directory containing `._*` files at all. If you copy the chart
to a node by hand, do the same:

```sh
COPYFILE_DISABLE=1 tar -C k8s --exclude '._*' -cf - prometheus grafana \
  | ssh <master> 'tar -C ~/aries-setup/charts -xf -'
```

`scp -r` is also affected. `rsync` is not, unless run with `-X`.

## Refreshing: what else to update

Update three things, or the install breaks in ways Helm will not report:

- `chart_version` in `../../setup/configs/prometheus/prom_config.json`.
  `aries-setup` refuses to install when it disagrees with `chart/Chart.yaml`.
- The table above, including the new digest.
- Every key in `values.yaml` and `../grafana/values.yaml`. **Helm ignores keys
  a chart does not define instead of failing**, so a key that upstream renamed
  silently reverts to its default.
