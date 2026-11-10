# Linux 5.10.239 attribution layouts

These source inputs were recovered from the amd205 worker. `base-layout.json`
was previously `runs/tsb-page-attribution-fix-git/layout/layout.json`;
`names-layout.json` and `owner-layout.json` came from the corresponding
`runs/tsb-page-attribution-five` layout files. Their bytes match the published
Firecracker companion layouts at commit
`209428517ba3f92388e31961183aedea089f6662` under
`tools/aries-attribution/layouts/linux-5.10.239/`.

`layout_offsets.c` and `offset-names.json` retain the compiler-generated base
layout field ordering. The JSON metadata records the guest kernel hash, source
revision and offset-validation limits. These offsets are specific to that kernel
and configuration. See the [observer guide](../../README.md#kernel-layouts-and-native-fixture)
for regeneration and validation commands.
