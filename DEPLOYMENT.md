# On-Device NetFlow Aggregator — Deployment Guide

This guide covers building, deploying, and operating the GoFlow2 + Aggregator container on MikroTik routers (ARM64).

## Overview

The container runs two processes via supervisord:

1. **GoFlow2** — IPFIX/NetFlow collector, writes raw JSON to `/var/log/flow.log`
2. **Aggregator** — Reads flow.log every 5 minutes, groups flows by `src|dst|port|direction|MAC`, writes compressed NDJSON gzip files to `/var/log/flows/`

A cron job on the server (fk-cron) SCPs the gzip files from the router and inserts them into ClickHouse.

## Build

```bash
# Build ARM64 image
docker buildx build --platform linux/arm64 --load -t goflow2-mikrotik .

# Export to tar for upload
docker save goflow2-mikrotik -o goflow2-mikrotik.tar
```

## Router Setup

### Prerequisites

- RouterOS 7 with container package enabled
- Sufficient flash storage (500MB+ recommended)

### 1. Enable Container Mode

```routeros
/system/device-mode/update container=yes
# Router will reboot
```

### 2. Create Network

```routeros
# veth interface for the container
/interface/veth/add name=veth1 address=172.17.0.2/24 gateway=172.17.0.1

# Bridge for container networking
/interface/bridge/add name=containers
/ip/address/add address=172.17.0.1/24 interface=containers
/interface/bridge/port/add bridge=containers interface=veth-goflow

# NAT for container internet access (optional)
/ip/firewall/nat/add chain=srcnat action=masquerade src-address=172.17.0.0/24
```

### 3. Create Container Mount

The aggregator writes gzip files to `/var/log/flows/` inside the container. This is bind-mounted to `flash/flows` on the router so fk-cron can SCP the files.

```routeros
/container/mounts/add name=flows src=flash/flows dst=/var/log/flows
```

### 4. Upload and Add Container

Upload `goflow2-mikrotik.tar` to the router via Winbox (drag to `flash/`).

```routeros
/container/add file=flash/goflow2-mikrotik.tar interface=veth-goflow root-dir=flash/goflow2 mounts=flows logging=yes
```

Then set memory limit and tmpdir:
```routeros
/container/config/set tmpdir=flash/tmp
/container/set 0 memory-max=100M
```

### 5. Configure Traffic Flow

Point the router's IPFIX export at the container:

```routeros
/ip/traffic-flow/set enabled=yes interfaces=bridge1,ether1-wan
/ip/traffic-flow/target/add dst-address=172.17.0.2 port=2055 version=9
```

Setting `interfaces=bridge1,ether1-wan` is required for MAC address fields (80/81) to be populated.

### 7. Start Container

```routeros
/container/start 0
```

## Verification

### Check Container Status

```routeros
/container/print
```

### Shell Into Container

```routeros
/container/shell 0
```

```bash
# Check goflow2 is receiving data (wait ~30 seconds)
wc -l /var/log/flow.log

# Check for errors
cat /var/log/goflow2_stderr.log
cat /var/log/aggregator_stderr.log

# After 5 minutes, check for gzip output files
ls -la /var/log/flows/
```

### Verify Files Visible From RouterOS

```routeros
/file/print where name~"flash/flows"
```

You should see `aggregated.YYYYMMDD_HHMMSS.json.gz` files.

## Aggregator Flags

| Flag | Default | Description |
|------|---------|-------------|
| `-input` | `/var/log/flow.log` | Raw flow log file (written by goflow2) |
| `-output-dir` | `/var/log/flows` | Directory for gzip output files |
| `-period` | `5` | Aggregation period in minutes |
| `-max-output-mb` | `500` | Max total size of output dir in MB (0=unlimited) |

These are configured in `cmd/aggregator/supervisord.conf`.

## Disk Safety

The container has no OS-level storage quota on internal NAND flash. Disk usage is bounded at the application level:

| Component | What caps it | Max size |
|-----------|-------------|----------|
| Gzip output (`/var/log/flows/`) | `-max-output-mb` flag, removes oldest files when exceeded | 500MB default |
| `flow.log` | Truncated after each 5-min aggregation cycle | ~5 min of raw JSON |
| Supervisord logs | `logfile_maxbytes=1MB`, `logfile_backups=1` per program | ~12MB total |
| Container RAM | `memory-max=100M` on container config | 100MB (OOM kill) |

### What happens when quota is exceeded

The aggregator's `enforceOutputQuota()` removes the **oldest** gzip files (by filename timestamp) until total size is back under the limit. Recent data is preserved.

### flow.log truncation

Every 5 minutes, the aggregator:
1. Reads new lines from `flow.log`
2. Writes aggregated gzip to `/var/log/flows/`
3. Truncates `flow.log` to zero

Truncation always runs, even if step 1 or 2 fails. If the aggregator process crashes before reaching truncation, supervisord restarts it (up to 5 retries).

## Data Retrieval (fk-cron)

The `netflow_agg_transport` cron job in fk-cron runs every 15 minutes:

1. Lists `.json.gz` files on the router via SSH: `/file print terse where name~"flash/flows" and name~".json.gz"`
2. SCPs each file to a local temp directory
3. Replaces `sampler_address` (Docker bridge IP `172.17.0.1`) with the router's real IP
4. Inserts NDJSON into ClickHouse `netflow.router_agg_flows` via `JSONEachRow`
5. Deletes successfully inserted files from router: `/file/remove "flash/flows/aggregated.xxx.json.gz"`

## Output Format

Each gzip file contains NDJSON (one JSON object per line):

```json
{
    "src_addr": "192.168.1.181",
    "dst_addr": "8.8.8.8",
    "port": 53,
    "direction": "outbound",
    "wan_mac": "04:f4:1c:4f:bb:a1",
    "lan_mac": "00:00:00:00:00:00",
    "total_bytes": 38489,
    "total_packets": 543,
    "flow_count": 537,
    "proto": "UDP",
    "sampler_address": "172.17.0.1",
    "first_seen_time": "2026-05-12T14:14:34.014161Z",
    "last_seen_time": "2026-05-12T14:19:15.86404596Z"
}
```

## Troubleshooting

### No data in flow.log

Traffic flow not targeting container IP. Verify:
```routeros
/ip/traffic-flow/print
/ip/traffic-flow/target/print
```

### Gzip files not visible from RouterOS

RouterOS creates a `.type` file in container mount directories after each restart, hiding contents from `/file/print` and SCP. fk-cron handles this automatically by deleting `.type` before listing files. For manual debugging: `/file/remove "flash/flows/.type"`

### wan_mac/lan_mac all zeros

Traffic flow `interfaces` not set. Fix:
```routeros
/ip/traffic-flow/set interfaces=bridge1,ether1-wan
```

### Aggregator errors parsing JSON

Check `cat /var/log/aggregator_stderr.log`. Usually means `mapping.yaml` is not loaded (goflow2 field names don't match aggregator struct). Verify `/etc/goflow2/mapping.yaml` exists inside the container.

### Container won't start

```routeros
/system/device-mode/print   # container=yes?
/file/print detail where name~"flash"  # enough space?
```

## Rollback

```routeros
/container/stop 0
/container/remove 0
/file/remove flash/goflow2
/file/remove flash/flows
# Remove traffic-flow target pointing at container
/ip/traffic-flow/target/remove [find dst-address=172.17.0.2]
```

## Updating the Container

```bash
# Rebuild
docker buildx build --platform linux/arm64 --load -t goflow2-mikrotik .
docker save goflow2-mikrotik -o goflow2-mikrotik.tar
```

Upload new tar to router via SCP, then update in-place (no need to re-enter config):
```routeros
/container/stop 0
# wait for status=stopped
/container/set 0 file=flash/goflow2-mikrotik.tar
/container/start 0
```

The `.type` file will be recreated on container start — fk-cron handles this automatically.
