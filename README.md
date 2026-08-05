# monitor-internet

Continuously monitors internet connection health on macOS and **attributes** each
failure to the network segment responsible — your own equipment, your provider's
access line, or their upstream transit.

The point is not a pretty graph. It is a defensible answer to one question:
*how much downtime is actually the provider's fault?*

## Why attribution, not just pinging

A loop that pings `8.8.8.8` records failures it cannot explain. A dead router, a
Wi-Fi drop, and a genuine provider outage all look identical to it, so every
number it produces is impeachable. This tool probes three tiers of the path every
cycle:

| Tier | Target | A failure here means |
|---|---|---|
| `gateway` | your router (auto-discovered) | **your own** equipment |
| `isp_edge` | first public hop past the router | the provider's access line |
| `internet` | Cloudflare, Google, Quad9 | reachability beyond the provider |

Verdicts, one per cycle:

| Verdict | Meaning | Counts against the ISP? |
|---|---|---|
| `OK` | everything answered | — |
| `LAN_FAULT` | router did not answer | **No** |
| `ISP_FAULT` | router up, nothing beyond it | **Yes** |
| `ISP_UPSTREAM` | ISP edge up, internet unreachable | **Yes** |
| `ICMP_DEPRIO` | edge hop silent, internet fine | No — cosmetic |
| `DEGRADED` | some anchors unreachable | No |

## Install

**macOS only.** Topology discovery shells out to `/sbin/route` and
`/usr/sbin/traceroute`, and the background jobs are launchd LaunchAgents. The
binary builds on other platforms but exits immediately with an explanation.

No root, no sudo, and no runtime dependency on Go or Homebrew — once built, the
binary carries everything it needs.

### With `go install`

Requires Go 1.24 or newer.

```sh
go install github.com/mohammadshamma/monitor-internet/cmd/mon@latest
mon install-agent     # install and load both launchd agents
```

`go install` places the binary in `$GOBIN` (or `$GOPATH/bin`, usually
`~/go/bin`) — make sure that is on your `PATH`. `mon install-agent` resolves the
running executable's real path, so the launchd agents point wherever the binary
actually landed; there is nothing to configure.

To upgrade, re-run `go install …@latest` then `mon restart-agent`.

### From source

```sh
git clone https://github.com/mohammadshamma/monitor-internet
cd monitor-internet
make install          # build + install ~/.local/bin/mon, restart agents if loaded
mon install-agent     # first time only
```

`ping` is never invoked — the binary opens its own unprivileged ICMP socket.

## Use

```sh
mon status                     # current live state
mon calibrate                  # baseline loss per target; picks the ISP hop
mon report --since 30d         # the report to cite at your provider
mon report --since 30d --csv   # or --json
mon config show                # effective configuration
```

The dashboard is served by the second launchd agent; open
`http://localhost:8765/` (or the host's LAN name from another device).

It opens the database **read-only**, so it cannot lock or corrupt the collector's
data, and runs as a separate job so a web fault costs no monitoring.

## Configuration

Everything machine-specific lives outside the repository, in
`~/.config/monitor-internet/config.json`. There is no config file by default —
the built-in defaults are complete and working.

```sh
mon config path     # where it goes
mon config init     # write the defaults there to edit
mon config show     # what is actually in effect
```

```json
{
  "interval": "5s",
  "timeout": "2s",
  "count": 3,
  "fail_threshold": 3,
  "clear_threshold": 3,
  "retain_samples": "0s",
  "trace_target": "1.1.1.1",
  "internet_anchors": [
    { "name": "cloudflare", "addr": "1.1.1.1" },
    { "name": "google",     "addr": "8.8.8.8" },
    { "name": "quad9",      "addr": "9.9.9.9" }
  ],
  "web": { "host": "0.0.0.0", "port": 8765 },
  "launchd_label_prefix": "net.monitorinternet"
}
```

| Key | Notes |
|---|---|
| `interval` | probe cycle. See the threshold caveat below before changing it. |
| `retain_samples` | `"0s"` keeps raw samples forever (the default). |
| `internet_anchors` | use independent operators; a quorum guards against one anchor having a bad day. |
| `gateway`, `isp_edge` | omit to auto-discover. Set them only for unusual networks (double NAT, a router that never answers ICMP). |
| `web.host` | `0.0.0.0` reaches the LAN; `127.0.0.1` is loopback only. |
| `launchd_label_prefix` | your own reverse-DNS namespace, if you prefer. |

`MONITOR_INTERNET_CONFIG` overrides the config path.

**Binding to `0.0.0.0` exposes a read-only, unauthenticated page to your local
network.** It carries ping timings, outage timestamps, your gateway IP and your
SSID — low sensitivity, no credentials — but set `web.host` to `127.0.0.1` if you
would rather it were loopback-only.

## How the numbers are kept honest

- **Debounce.** An outage opens only after `fail_threshold` consecutive bad
  cycles and closes after `clear_threshold` good ones. One dropped packet cannot
  manufacture an outage. Failures shorter than ~15s are not recorded at all.
- **Backdating.** An outage's start is the first bad cycle, not the one that
  crossed the threshold; its end is the last bad cycle, not the moment recovery
  was noticed. Otherwise every duration would be systematically wrong.
- **Coverage.** Every cycle writes a heartbeat. A gap means *the monitor was not
  running* — that time is excluded from both uptime and downtime and reduces
  **coverage** instead. Always read availability alongside coverage; an
  availability figure without one is unfalsifiable.
- **ICMP de-prioritisation.** Routers routinely rate-limit pings aimed at their
  own interface. A silent `isp_edge` never opens an outage on its own; it is only
  ever corroborating.
- **Escalation.** An outage that starts as `LAN_FAULT` but proves to be the
  provider is rewritten to `ISP_FAULT` on close.

> **Thresholds are counted in cycles, not seconds.** At the default 5s interval,
> 3 cycles is ~15s. If you raise `interval` to 30s, the same 3 cycles becomes 90s
> before an outage registers. Adjust the thresholds to match.

## Known limitation: router WAN faults look like ISP faults

`LAN_FAULT` only fires when the router stops answering entirely. If your router
answers fine but *its own WAN side* has failed — a dying modem/ONT, a dropped
PPPoE session, a flaky WAN port — that presents as `ISP_FAULT`, because from the
monitoring host it is indistinguishable from the provider's line being down.

This matters for the decision the tool exists to inform: if your own modem is at
fault, changing providers will not help. Ways to close the gap, none implemented:

- If the modem/ONT is a separate box from the router, add it as a fourth tier —
  that splits "my modem" from "the provider's line".
- Many routers expose WAN status or an uptime counter; polling it would
  disambiguate directly.

Treat `ISP_FAULT` as "the fault is at or beyond the router's WAN side."

## Data

```
~/.local/share/monitor-internet/monitor.db   SQLite, WAL
~/.local/state/monitor-internet/log/         collector.log, web.log
```

Per cycle: one heartbeat row (timestamp, verdict, interface, SSID) and one sample
row per target (`ok`, `rtt_us`, `loss_pct`). `rtt_us` is the **minimum** of the
echoes sent, being least contaminated by scheduler noise.

**Nothing is deleted by default.** Latency history is only useful in hindsight,
and deleting it forecloses questions not yet asked. Growth is ~52 bytes per
sample row — at the default interval and five targets, about **4.3 MB/day** or
**1.6 GB/year**. Set `retain_samples` to enable pruning.

Because samples are kept indefinitely, the latency chart's queries are
cost-bounded: past ~400k rows they stride over the data rather than reading all of
it, and the dashboard says so explicitly when that happens.

## Startup at boot

Two user LaunchAgents, no root:

| Label | Role |
|---|---|
| `<prefix>.collector` | probe loop — must never die |
| `<prefix>.web` | dashboard |

A LaunchAgent loads at *user login*. On a machine with **auto-login enabled and
FileVault off**, a reboot lands in a live user session with nobody present, so the
agents start unattended. A screen lock does not unload them.

> **With FileVault on, or auto-login off, the agents will not start until someone
> logs in.** That window is correctly recorded as unmonitored — reducing coverage,
> not counted as an outage — but on a headless box it is the one configuration
> detail that could quietly undermine the dataset. Check `mon report` coverage
> after any reboot.

```sh
launchctl print gui/$(id -u)/net.monitorinternet.collector   # inspect
mon restart-agent                                             # after a rebuild
mon uninstall-agent                                           # remove
```

## Why Go

The binary carries every dependency inside it and links nothing but `libSystem`
and system frameworks — verify with `otool -L $(which mon)`. Uninstalling Go,
upgrading Homebrew, or wiping the module cache cannot affect an already-built
binary.

`/sbin/ping` is not setuid on macOS yet works as an ordinary user, which proves
the OS grants unprivileged ICMP datagram sockets. The collector uses one
directly: no subprocess per probe, no scraping ping's text output, and no
process-startup jitter polluting the RTT measurements.

`traceroute`, `route` and `networksetup` are still shelled out for topology
discovery, but they run rarely, never in the timing path, and ship with macOS.

Dependencies are not vendored in this repo (they are ~140 MB). `go.mod`/`go.sum`
pin exact versions; run `make vendor` if you want offline rebuilds.

## Tests

```sh
make check    # go vet + go test ./...
```

The end-to-end tests in `internal/collector` drive real ICMP sockets, real SQLite
and the real classifier, using TEST-NET-1 (`192.0.2.0/24`, guaranteed unroutable)
to simulate dead tiers without touching your network. The one that matters most is
`TestDeadGatewayIsNeverBlamedOnTheProvider`.

### Verifying attribution on your own machine

The claim worth checking before you rely on any of this is that a fault in *your*
equipment never lands on the provider's record. `scripts/wifi-drop-test` proves it
end to end by actually cutting the link:

```sh
mon report --since 24h          # note ISP downtime and ISP outage count
nohup scripts/wifi-drop-test en1 60 >/dev/null 2>&1 &
# …wait for it to come back, then:
mon report --since 24h          # those two numbers must be unchanged
```

A new `LAN_FAULT` outage should appear, LAN downtime should grow by roughly the
window, and **ISP downtime and ISP outage count must not move at all**. Note that
ISP *availability* may still rise slightly: it is a share of monitored time, so a
constant downtime over a larger denominator gives a better percentage.

> ⚠️ **This deliberately takes the machine off the network.** Run it detached
> (`nohup … &`) — the link it cuts is very likely the one your SSH session is on.
> Restoration runs on two independent paths: the main sequence retries and
> verifies power came back and DHCP returned an address, and a failsafe armed
> *before* the link is touched re-enables Wi-Fi unconditionally at
> `window + 120s` even if the main sequence is killed. Progress is logged to
> `/tmp/wifi-drop-test.log`.

Measured on a Mac mini: a 60s Wi-Fi drop produced a 60s `LAN_FAULT` outage,
backdated to the first failing cycle, with the provider's downtime and outage
count byte-identical before and after.

## License

MIT
