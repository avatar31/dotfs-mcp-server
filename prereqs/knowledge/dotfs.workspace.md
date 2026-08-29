# Halmidi Engineering Context & Agent Instruction Manual

This document serves as the master architectural reference and agent skill profile for **Halmidi**—an enterprise-grade, multi-protocol Software-Defined Storage engine unifying AWS S3, NFS (NFS-Ganesha), and SMB (Samba) over a shared Go storage runtime.

---

## 1. System Identity & Core Tenets

* **Project Name:** Halmidi
* **Target Domain:** Multi-Protocol Software-Defined Storage for Unstructured Data
* **Core Languages:** Go (Core Daemon, S3 Gateway, Metadata, Chunking) and C (NFS-Ganesha FSAL, Samba VFS)
* **Core Mission:** Provide simultaneous object and file access to the same underlying dataset without dual-writes, sync delays, or POSIX/S3 semantic mismatches.

---

## 2. Process & Runtime Topology
```
┌─────────────────────────┐          ┌─────────────────────────┐
│     NFS Ganesha (C)     │          │        Samba (C)        │
│    [Custom FSAL_DOTFS]  │          │    [Custom VFS_DOTFS]   │
└───────────┬─────────────┘          └───────────┬─────────────┘
            │                                    │
    (UDS: nfs.sock)                      (UDS: smb.sock)
            │                                    │
┌───────────▼────────────────────────────────────▼─────────────┐
│                    HALMIDI PROCESS (Go)                      │
│                                                              │
│  ┌────────────────────────────────────────────────────────┐  │
│  │                    Access & Protocol Layer             │  │
│  │  [ S3 REST Gateway ] [ NFS UDS Handler ] [ SMB Handler]│  │
│  └───────────┬──────────────────┬─────────────────┬───────┘  │
│              │                  │                 │          │
│              ▼                  ▼                 ▼          │
│  ┌────────────────────────────────────────────────────────┐  │
│  │               Core Storage & Abstraction VFS           │  │
│  │  • Unified Inode/Key Namespace   • DLM & Lease Engine  │  │
│  │  • Canonical ACL & IAM Resolver  • Multi-part Manager  │  │
│  └──────────────────────────────┬─────────────────────────┘  │
│                                 │                            │
│              ┌──────────────────┴──────────────────┐         │
│              ▼                                     ▼         │
│  ┌────────────────────────┐         ┌─────────────────────┐  │
│  │   Omashu Metadata DB   │         │ dotfs Storage Engine│  │
│  │ (Raft + BadgerDB K/V)  │         │ (EC, Chunking, VFS) │  │
│  └────────────────────────┘         └──────────┬──────────┘  │
│                                                │             │
└────────────────────────────────────────────────┼─────────────┘
                                                 ▼
                                     [ Raw NVMe / SSD / HDD ]
```

---

## 3. Component Deep-Dive & Responsibilities

### 1. Halmidi Core (`github.com/avatar31/halmidi`)
* **Role:** Primary storage daemon and orchestration runtime in Go.
* **Local Path:** `./halmidi/`
* **Sub-Modules:**
  * **S3 REST Engine:** Implements the AWS S3 REST API (SigV4, Bucket/Object CRUD, Multipart uploads).
  * **UDS Protocol Handlers:** Dedicated goroutines terminating bidirectional Unix Domain Sockets (`nfs.sock`, `smb.sock`) from C gateways.
  * **Distributed Lock Manager (DLM):** Coordinates cross-protocol file locking, SMB oplocks/leases, and POSIX `fcntl` reservations.
  * **Canonical ACL & Identity Engine:** Maps AWS IAM policies $\leftrightarrow$ POSIX UID/GID $\leftrightarrow$ Windows Security Descriptors (SIDs).
  * **Background Workers:** Hosts Bitrot Scrubber, EC Reconstruction, Garbage Collection, and Quota enforcement.

### 2. Omashu (`github.com/avatar31/omashu`)
* **Role:** Linearizable, distributed metadata database embedded inside the Halmidi process.
* **Local Path:** `./omashu/`
* **Engine:** BadgerDB engine wrapped with a Raft consensus state machine.
* **Stored Schemas:**
  * Inode allocation tables and parent-child directory hierarchies.
  * Flat S3 bucket and object index mapping.
  * In-flight multipart upload staging manifests.
  * Active distributed lease/lock state tables.

### 3. dotfs (`github.com/avatar31/dotfs`)
* **Role:** Low-level chunk storage engine and disk abstraction.
* **Local Path:** `./dotfs/`
* **Functions:**
  * Reed-Solomon Erasure Coding ($K+M$) data/parity chunk generation.
  * Chunk placement across disk topologies and failure domains.
  * Checksum verification (BLAKE3/CRC32C) per chunk read/write.
  * Block allocation, direct I/O, and raw volume disk persistence.

### 4. NFS-Ganesha (`github.com/avatar31/nfs-ganesha`)
* **Role:** [Fork of [nfs-ganesha](https://github.com/nfs-ganesha/nfs-ganesha)] User-space NFSv3/v4 server daemon.
* **Local Path:** `./nfs-ganesha/`
* **Integration Module:** Custom File System Abstraction Layer plugin (`FSAL_DOTFS`).
* **Mechanism:** Converts POSIX filesystem calls (`lookup`, `read`, `write`, `setattr`) into binary IPC messages forwarded across `nfs.sock` to Halmidi.

### 5. Samba (`github.com/avatar31/samba`)
* **Role:** [Fork of [samba](https://github.com/samba-team/samba)]User-space SMB2/SMB3 server daemon for Windows clients.
* **Local Path:** `./samba/`
* **Integration Module:** Custom Samba Virtual File System module (`vfs_dotfs`).
* **Mechanism:** Hooks into file access, handle creation, oplock/lease negotiations, and Windows ACL checks, forwarding commands via `smb.sock` to Halmidi.

---

## 4. Key Cross-Protocol Invariants

* **Single Source of Truth:** `Omashu` is the sole authority for metadata state; `dotfs` is the sole authority for chunk byte storage.
* **Instant Parity:** Any file written via NFS/SMB must immediately be listable and GET-able via S3 at its corresponding URI key without external synchronization jobs.
* **Lease Arbitration:** Before an S3 PUT/DELETE or NFS write commits on an active file, Halmidi must evaluate Omashu's lock table. If an active SMB exclusive write lease exists, a lease break notification is dispatched over `smb.sock` before data modification.
* **Multipart Staging Isolation:** Uncommitted S3 multipart parts reside in a hidden Omashu metadata namespace and are invisible to POSIX `readdir` until `CompleteMultipartUpload` issues an atomic Raft metadata commit.
* **IPC Transport Protocol:** Gateway-to-Halmidi communication uses framed binary packets over Unix Domain Sockets (UDS) (32-byte header: `OpCode`, `TxID`, `InodeID`, `PayloadLen`, `CRC32C`), with optional shared-memory (`shm_open`) zero-copy buffers for large payloads.

---

## 5. Agent Skills & Task Routing

When asked to reason about, modify, or extend Halmidi SDS, apply these specialized rules:

1. **Protocol Gateway Tasks (C / Systems):**
   * FSAL hooks (`ganesha/`) or Samba VFS hooks (`samba/`) must remain thin wrappers. Do not embed heavy storage logic in C gateways; forward requests via UDS framing to Halmidi.
2. **S3 & REST Logic (Go):**
   * Keep S3 streaming zero-copy where possible. Parts and objects stream directly into `dotfs` chunks while Omashu stores only the manifests/inodes.
3. **Metadata & Raft State Machine (Go):**
   * All mutations to directory hierarchies, link counts, object mappings, or lock states must execute as linearizable Raft transactions in Omashu.
4. **Resiliency & EC (Go / C):**
   * Treat chunk writes in `dotfs` as immutable chunk units protected by Reed-Solomon parity and checksum verification. Always consider failure recovery and background repair overhead.
