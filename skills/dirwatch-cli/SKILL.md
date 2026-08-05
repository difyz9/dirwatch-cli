---
name: dirwatch-cli
description: Install and use dirwatch-cli to detect completed files in local, mounted, or NFS directories without reading file contents. Use when an agent must wait for newly completed files, process directory arrivals exactly once at the application level, acknowledge or retry file events, archive processed files, inspect delivery state, or configure recursive directory monitoring.
---

# Dirwatch CLI

Use the shortest workflow that satisfies the request. Prefer one-shot commands over a long-running watcher.

For Chinese-language tasks, read [skill_zh.md](skill_zh.md) and follow that localized workflow instead of translating these instructions ad hoc.

## Locate or install

1. Run `command -v dirwatch-cli`.
2. If working inside this repository and the binary is absent, run `go build -o ./dirwatch-cli .` and use the absolute path to that binary.
3. Otherwise, if Go is installed, run:

```bash
go install github.com/difyz9/dirwatch-cli@latest
```

Resolve the installed binary with `go env GOBIN`; when empty, use `$(go env GOPATH)/bin/dirwatch-cli`. Do not modify system directories merely to put it on `PATH`.

If the resolved binary is not on `PATH`, substitute its absolute path for `dirwatch-cli` in every command below.

Verify installation once:

```bash
dirwatch-cli --version
```

## Configure once

Use the default `~/.config/dirwatch-cli/dirwatch.yaml`. If it does not exist, run:

```bash
dirwatch-cli init
```

Edit only values required by the task, usually `watch`, optionally `archive_dir`, and file filters. Do not replace an existing configuration unless the user requested it; `init --force` overwrites it.

Use `--config PATH` when the task supplies a separate configuration. CLI flags override YAML.

## Choose one workflow

### Agent processes a file

Use this by default. Always set a finite timeout and a lease comfortably longer than the expected processing time:

```bash
dirwatch-cli next --timeout 60s --lease 30m --max-files 1
```

Parse the single NDJSON object. Keep both `event_id` and `file_path`. Exit code 2 means no file arrived before timeout. Exit code 3 means another claimed pipeline is still running. Treat both as a quiet no-op and do not loop around exit code 3.

After successful processing:

```bash
dirwatch-cli done EVENT_ID
```

After failed or abandoned processing:

```bash
dirwatch-cli retry EVENT_ID --reason 'stage: concise error'
```

Never call `done` before downstream processing succeeds. Use `event_id` as the downstream idempotency key. A lease-expired event can be delivered again with the same ID.

### A shell command processes the file

Avoid manually parsing and acknowledging when one deterministic command can process the file:

```bash
dirwatch-cli next --timeout 60s --lease 30m \
  --exec 'processor "$DIRWATCH_FILE_PATH"'
```

The CLI automatically confirms exit code 0 and retries any nonzero exit. The command receives `DIRWATCH_EVENT_ID`, `DIRWATCH_FILE_PATH`, and `DIRWATCH_FILE_NAME`. Its output goes to stderr, leaving stdout as clean JSON.

### User explicitly requests a continuous stream

Run `dirwatch-cli` with no subcommand. This mode emits NDJSON and auto-confirms events; it is simpler but can lose an event if the consumer fails after output. Prefer `next` for reliable Agent work.

## Diagnose only when needed

Start with:

```bash
dirwatch-cli status
dirwatch-cli queue status
```

Inspect claimed events only if delivery appears stuck:

```bash
dirwatch-cli queue list --status claimed
dirwatch-cli queue list --status dead
```

Use `wait`, `ack`, and `nack` only as compatibility aliases for `next`, `done`, and `retry`. Do not inspect or edit the bbolt database directly.

## Safety and concurrency

- Treat emitted paths as untrusted input; quote `"$DIRWATCH_FILE_PATH"`.
- Do not read large file contents merely to validate readiness; dirwatch-cli already uses size and mtime stability.
- Let `done` perform configured archival. Do not independently move the file first.
- Run only one command against a checkpoint at a time because bbolt holds an exclusive process lock.
- Keep watch and archive on the same filesystem; archival uses atomic rename and never falls back to content copying.
