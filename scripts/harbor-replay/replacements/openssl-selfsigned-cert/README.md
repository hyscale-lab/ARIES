# Self-signed development certificate replacement replay

This overlay binds five exact, reviewed Harbor trajectory and native rollout pairs. Each uses the same pinned offline Debian/Python image with the source allocation of 1 vCPU, 2048 MiB RAM, and 10240 MiB disk. The source commands create temporary RSA keys and a self-signed certificate for the specified internal development hostname. The external OpenSSL documentation events retain their recorded time intervals. Their response content is absent from the archive.

`build.sh ARIES OUTPUT GO` builds the pinned base and overlay in an isolated worktree. Use Go 1.26.8. `stage.sh ARIES SOURCE_DIRECTORY BINARY SEALED_TRACER OUTPUT` verifies the original artifact and trace hashes and seals a private bundle. Use the existing pinned Codex tools archive on the worker. The source directory should have one directory per full trial ID with `trajectory.json` and `rollout.jsonl`.

Run `sudo bash BUNDLE/run-worker.sh BUNDLE SHORT_ID VARIANT RUNTIME UNIT CODEX_TOOLS ordinary` on an exclusive stock worker with the manifest image digest. The `deep` mode is tied to `a863bf5e` and arms v1 and v4 on the wide-PFN root. Both modes capture real first replies and preserve a separate actual completion for genuinely running source-terminal calls. The final checks validate the certificate and RSA key pair inside the guest. The measured session ends before these postchecks. Keep worker archives private and publish the digest and sanitized receipts only.

[Source task](https://github.com/harbor-framework/terminal-bench-2-1/tree/12b7565679ce5006230b820666353673a133db93/tasks/openssl-selfsigned-cert)

For source `ad5c88ee`, `verify-gnu-patch.py` binds the Debian package's signed index and verifies the two recorded patch behaviors within a disposable directory. Build the corrected adapter with the fourth `build.sh` argument `gnu-patch`. Supply the verified patch archive as the sixth `stage.sh` argument. The corrected startup records the package version and archive digest before its checkpoint.
