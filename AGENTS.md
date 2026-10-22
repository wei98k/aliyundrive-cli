# Agent Guide

This is an independent Go CLI. Do not modify ../src/aliyundrive-cli.

## Build And Verify

```sh
go test ./...
go test -race ./...
go vet ./...
go build -o bin/aliyundrive ./cmd/aliyundrive
```

## CLI Contract

- Run `aliyundrive schema` for machine-readable commands and flags.
- Each invocation emits exactly one JSON envelope to stdout, including errors and help.
- Never prompt for input or silently start a QR login from a file command.
- Remote references are absolute paths, `root`, or `id:FILE_ID`; never depend on a working directory.
- Prefer IDs returned by `ls`/`search` when names are ambiguous.
- Use `--dry-run` before mutations; `trash` requires `--yes` unless dry-running.
- Do not expose access/refresh tokens, signed upload URLs, or raw upstream bodies in output.
- Credentials belong outside the repository with directory mode 0700 and file mode 0600.
- Do not run live mutations without a user-requested target. Mock HTTP tests are the default.
- This uses the web API family, not the registered-application Open Platform OAuth API. The full real-account workflow passed on 2026-10-22; future upstream changes still require revalidation.
- Opt-in live tests require ALIYUNDRIVE_LIVE_TEST_CONFIG and ALIYUNDRIVE_LIVE_ALLOW_WRITE=1. They only mutate a random test directory and move it to the recycle bin afterward. Never enable them implicitly.
