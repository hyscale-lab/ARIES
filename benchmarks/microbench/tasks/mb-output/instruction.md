Run exactly one shell command, exactly as written, in a single tool call. It prints about 64 KB:

```sh
head -c 49152 /dev/zero | base64 | tee /tmp/mb-out
```

Do not run anything else. Then reply with exactly: DONE
