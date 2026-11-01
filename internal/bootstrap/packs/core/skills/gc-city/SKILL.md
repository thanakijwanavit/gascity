---
name: gc-city
description: City lifecycle — status, start, stop, init
---

# City Lifecycle

A city is a directory containing `city.toml` and `.gc/` runtime state.

## Initialization

```
gc init                                # Initialize city in current directory
gc init <path>                         # Initialize city at path
```

## Starting and stopping

```
gc start                               # Start city under the supervisor
gc start <path>                        # Start city at path under the supervisor
gc supervisor run                      # Run the supervisor in the foreground
gc start --dry-run                     # Preview what would start
gc stop                                # Stop the current city
gc restart                             # Stop then start
```

`gc init` and `gc start` register the city with the machine supervisor,
ensure it is running, and trigger an immediate reconcile. Interactive
sessions are created separately with `gc session new <template>`.

## Status

```
gc status                              # City-wide overview
gc session list                        # Session / agent status
gc rig status <name>                   # Rig status
```

## Suspending

```
gc suspend                             # Suspend entire city
gc resume                              # Resume suspended city
```

## Configuration

```
gc config show                         # Show resolved configuration
gc config explain                      # Show config layering and provenance
gc doctor                              # Run health checks
```

## Events

```
gc events                              # List events (JSON Lines), then exit
gc events --type bead.created --since 1h   # Filter by type and age
gc events --follow                     # Stream new events until interrupted
gc events --watch --type convoy.closed --timeout 5m  # Block until a match
gc events --seq                        # Print the current cursor (plain text)
gc event emit <type> [data]            # Emit a custom event
```

Scope: inside a city (or with `--city`) you read that city's events; with no
city in scope you read the supervisor's merged bus, and each line adds a
`city` field.

Output: list, `--watch` and `--follow` print **JSON Lines**, one object per
line, no wrapper; no match prints nothing (not an error). `--seq` is the one
exception: a single plain-text cursor, never JSON. In a city it is a sequence
number (`21`) for `--after`; on the supervisor it is a composite cursor
(`__supervisor__:42,mc-city:21`) for `--after-cursor`. Heartbeats are never printed.

Resume without replaying history:

```
cur=$(gc events --seq)                 # before the step you want to observe
gc events --follow --after "$cur"      # city scope
gc events --follow --after-cursor "$cur"   # supervisor scope (no city)
```

Async API calls (for example `POST /v0/city`) return `202` with
`request_id` and `event_cursor`. Pass that `event_cursor` to
`--after-cursor` and read until a line's `payload.request_id` equals your
`request_id` with type `request.result.*` or `request.failed`. Without a
cursor a stream starts at the head and can miss the completion.

`--payload-match key=value` (dotted paths allowed) only filters; it never
changes the line shape. A path that does not resolve just matches nothing.
For `bead.*` events use `bead.issue_type=task` on a running city (API) but
`issue_type=task` on a stopped one (raw journal).

Full contract: `docs/reference/events.md`, async flows: `docs/reference/api.md`,
line schema: `docs/reference/schema/events.json`.

## Dashboard

See the gc-dashboard skill for full dashboard reference.

## Packs

Packs extend Gas City with additional commands, prompts, formulas, and
doctor checks. Pack commands appear as top-level `gc <pack> <command>`
subcommands.

```
gc pack list                           # List installed packs
gc pack fetch                          # Fetch remote packs
```
