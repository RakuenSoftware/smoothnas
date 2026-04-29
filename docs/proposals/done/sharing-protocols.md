# Proposal: Sharing Protocols (SMB, NFS, iSCSI)

**Status:** Completed
**Depends on:** mdadm-storage-tiering (the base appliance must exist first)

---

## Problem

The storage tiering appliance creates and manages tiered volumes, but provides no way to share them over the network. Without sharing protocols, the appliance is only useful for local storage on the host itself. To function as a NAS, it needs to export volumes via the protocols that clients actually use: SMB for Windows and mixed environments, NFS for Linux/Unix environments, and iSCSI for block-level access (VMs, databases, hosts that need raw devices).

---

## Goals

1. SMB file sharing via Samba, managed through the existing web UI
2. NFS exports via nfs-kernel-server, managed through the existing web UI
3. iSCSI block targets via LIO (targetcli), managed through the existing web UI
4. Per-volume sharing configuration (a volume can be shared via multiple protocols simultaneously, except iSCSI which is mutually exclusive with local mount)
5. Firewall rules managed automatically per enabled protocol

---

## Non-goals

- Active Directory or LDAP integration. Authentication is local accounts (SMB), IP-based (NFS), or CHAP (iSCSI).
- Kerberos/NFSv4 security. NFSv4 is supported but without Kerberos.
- Multi-path iSCSI or MPIO configuration.
- SMB clustering or CTDB.
- Automated client-side configuration or discovery.

---

## Architecture

### Package additions to the OS image

| Package | Protocol | Purpose |
|---------|----------|---------|
| `samba` | SMB | SMB server, `smbd` and `nmbd` services |
| `nfs-kernel-server` | NFS | NFS server, `nfsd` and related services |
| `targetcli-fb` | iSCSI | LIO iSCSI target management CLI |
| `python3-rtslib-fb` | iSCSI | LIO runtime library (used by targetcli) |
| `nftables` | All | Firewall rule management |

### Service and port layout

| Port | Protocol | Service | Opened when |
|------|----------|---------|-------------|
| 445 | TCP | SMB (`smbd`) | SMB sharing enabled |
| 2049 | TCP | NFS (`nfsd`) | NFS sharing enabled |
| 111 | TCP/UDP | rpcbind | NFS sharing enabled (NFSv3 compatibility) |
| 3260 | TCP | iSCSI (LIO) | iSCSI sharing enabled |

All sharing services are disabled by default. Enabling a protocol starts its service(s) and opens the corresponding firewall port(s). Disabling stops the service(s) and closes the port(s).

### How tierd manages each protocol

**SMB:** `tierd` generates `/etc/samba/smb.conf` from its internal state. Each share maps to a volume's mount point. After any change, `tierd` calls `smbcontrol all reload-config`. User accounts are synced to Samba's password database via `smbpasswd` (non-interactive mode) whenever a user is created, deleted, or changes their password through the web UI.

**NFS:** `tierd` generates `/etc/exports` from its internal state. Each export maps to a volume's mount point with specified network access. After any change, `tierd` calls `exportfs -ra` to reload.

**iSCSI:** `tierd` manages LIO targets via `targetcli` commands. Each iSCSI target exposes an LV as a block device. `tierd` uses the saveconfig/restoreconfig mechanism so targets persist across reboots. Because iSCSI exposes raw block devices, a volume exported via iSCSI must not be mounted locally. `tierd` enforces this: mounting an iSCSI-exported volume is rejected, and exporting a locally-mounted volume via iSCSI is rejected. The user must explicitly unmount before exporting, or remove the iSCSI target before mounting.

### Firewall management

`tierd` manages `nftables` rules. On startup, it reads which protocols are enabled and opens the appropriate ports. When a protocol is enabled or disabled, the rules are updated and applied via `nft -f`. The base ruleset allows only SSH (22) and HTTPS (443). Sharing protocol ports are added/removed dynamically.

---

## API

### Protocol management

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/protocols` | GET | List all protocols with enabled/disabled status and service health |
| `/api/protocols/{protocol}` | PUT | Enable or disable a protocol (`smb`, `nfs`, `iscsi`). Starts/stops service, opens/closes firewall port. |

### SMB shares

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/smb/shares` | GET | List all SMB shares |
| `/api/smb/shares` | POST | Create a share (volume ID, share name, read-only flag, allowed users, guest access) |
| `/api/smb/shares/{id}` | GET | Share details and connected clients |
| `/api/smb/shares/{id}` | PUT | Update share settings |
| `/api/smb/shares/{id}` | DELETE | Remove share |

SMB user access is tied to the existing local account system. When a user is created or their password changes, `tierd` automatically updates Samba's password database.

### NFS exports

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/nfs/exports` | GET | List all NFS exports |
| `/api/nfs/exports` | POST | Create an export (volume ID, allowed networks, sync/async, root_squash/no_root_squash, NFSv3/v4) |
| `/api/nfs/exports/{id}` | GET | Export details and connected clients |
| `/api/nfs/exports/{id}` | PUT | Update export settings |
| `/api/nfs/exports/{id}` | DELETE | Remove export |

### iSCSI targets

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/iscsi/targets` | GET | List all iSCSI targets |
| `/api/iscsi/targets` | POST | Create a target (volume ID, IQN, CHAP username/password). Unmounts the volume if mounted. |
| `/api/iscsi/targets/{id}` | GET | Target details, LUN mapping, connected initiators |
| `/api/iscsi/targets/{id}` | PUT | Update target settings (CHAP credentials, initiator ACLs) |
| `/api/iscsi/targets/{id}` | DELETE | Remove target and LUN mapping. Does not re-mount the volume. |
| `/api/iscsi/targets/{id}/acls` | GET | List allowed initiator IQNs |
| `/api/iscsi/targets/{id}/acls` | POST | Add an initiator ACL |
| `/api/iscsi/targets/{id}/acls/{acl}` | DELETE | Remove an initiator ACL |

### IQN naming convention

Targets are named automatically: `iqn.2026-01.com.smoothnas:{hostname}:{volume-name}`

The user can override the IQN at creation time.

---

## UI

### New pages

| Page | Description |
|------|-------------|
| **Sharing** | Overview of all protocols: enabled/disabled toggle, service health, count of shares/exports/targets per protocol |
| **SMB Shares** | Create, edit, delete SMB shares. Shows connected clients per share. |
| **NFS Exports** | Create, edit, delete NFS exports. Shows connected clients per export. |
| **iSCSI Targets** | Create, edit, delete iSCSI targets. Shows LUN mapping, initiator ACLs, connected initiators. Warning banner when creating (explains volume will be unmounted). |

### Volume page integration

The existing Volumes page gains a "Sharing" column showing icons/badges for each active protocol on that volume. Clicking opens the relevant share/export/target detail.

---

## Authentication per protocol

| Protocol | Auth mechanism | Managed by |
|----------|---------------|------------|
| SMB | Samba user/password (synced from local accounts) | Automatic: `tierd` calls `smbpasswd` when users change |
| NFS | IP/subnet allowlist per export | User configures per export via UI |
| iSCSI | CHAP (username/password per target) | User configures per target via UI. Credentials stored in tierd's SQLite database. |

---

## Trade-offs

**Samba user sync vs. independent Samba users.** Syncing from the web UI's local accounts keeps one source of truth. The downside is that every web UI user becomes a potential SMB user. The alternative (independent Samba user management) adds a second user database. Syncing is simpler and matches the single-host appliance model.

**No Kerberos/AD.** Keeps the appliance self-contained with no external dependencies. Users who need AD-joined SMB shares are outside the target audience for this appliance. Can be revisited in a future proposal.

**NFSv3 support (rpcbind on port 111).** NFSv4 alone would be cleaner (single port), but many Linux clients still default to NFSv3. Supporting both avoids client compatibility issues. Users who want to lock down to NFSv4-only can disable NFSv3 in the NFS export settings.

**iSCSI mutual exclusion with local mount.** This is a hard constraint, not a design choice. A block device cannot be safely mounted locally and exported as an iSCSI target simultaneously (dual writers corrupt the filesystem). `tierd` enforces this at the API level.

**nftables vs. iptables.** Debian 13 defaults to nftables. Using the native firewall framework avoids compatibility layers.

**CHAP for iSCSI vs. no auth.** CHAP is weak by modern standards but is the universal iSCSI authentication mechanism. Requiring it by default prevents accidental open access to block devices. Users on a trusted network can set trivial credentials.
