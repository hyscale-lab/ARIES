Run exactly one shell command, exactly as written, in a single tool call. It takes a few seconds:

```sh
python3 -c "import functools, hashlib; open('/tmp/mb-cpu', 'w').write(functools.reduce(lambda h, _: hashlib.sha256(h).digest(), range(3000000), b'aries').hex())"
```

Do not run anything else. Then reply with exactly: DONE
