# duolingo-watcher

Polls [Duolingo's status page](https://status.duolingo.com/) until the main Duolingo component is operational again.

## Build

```bash
go build -o duolingo-watcher .
```

## Usage

One-shot check (exit `0` if up, `1` if down):

```bash
./duolingo-watcher -once
```

Watch until recovered (default: every 30s, plays a sound + notification, blocks sleep):

```bash
./duolingo-watcher
```

Test the recovery alert (sound + notification) without waiting:

```bash
./duolingo-watcher -test-alert
```

Faster polling:

```bash
./duolingo-watcher -interval 15s
```

Only print when status changes:

```bash
./duolingo-watcher -quiet
```

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-once` | `false` | Check once and exit |
| `-interval` | `30s` | Poll interval while watching |
| `-notify` | `true` | Desktop notification when back online |
| `-sound` | `true` | Loop loud alarm on all sinks until Enter/Ctrl+C |
| `-inhibit` | `true` | Block idle/sleep via `systemd-inhibit` while watching |
| `-quiet` | `false` | Only print on status changes |
| `-test-alert` | `false` | Play recovery sound/notify and exit |
| `-component` | `Duolingo` | Component that must be `operational` |
| `-url` | status API summary | Override status JSON URL |

Uses `https://status.duolingo.com/api/v2/summary.json` (Statuspage). Sound uses `paplay` with the freedesktop alarm/complete sounds. Sleep inhibit uses `systemd-inhibit --what=idle:sleep`.
