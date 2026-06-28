# Technical Concepts Glossary

Concepts the dissertation needs to explain ("what is GSO/ECN, user vs kernel space, why this
why that"). Each entry: what it is, why it matters *to this project*, and where it shows up
in the code/results. Keep these accurate — they are viva-defensible.

## QUIC (the protocol under test)
A transport protocol that runs **over UDP** and builds in what TCP+TLS provide separately:
reliable ordered delivery, congestion control, and **TLS 1.3 encryption integrated into the
handshake**. Standardised in RFC 9000 (2021); it is the transport beneath HTTP/3. Because it
lives over UDP in **user space**, it can evolve without kernel/OS changes, but it pays a CPU
cost for doing in software what TCP does in the kernel/NIC. In this project QUIC is provided
by the `quic-go` library. Key QUIC features exploited here:
- **Integrated handshake (~1 RTT):** connection + encryption ready in one round trip
  (dimension 1 win).
- **Stream multiplexing with no head-of-line blocking** (dimension 3 win).
- **0-RTT resumption:** a returning client can send data in the very first packet using a
  cached session ticket (`Allow0RTT: true` in the config; demonstrated in `raw_mini/quic_0rtt.go`).
- **Connection migration / larger flow-control windows** (dimension 2 large-payload win).

## TCP (the baseline)
The decades-old kernel-implemented transport. Reliable, ordered, congestion-controlled, and
**heavily optimised in the OS kernel and offloaded to NIC hardware**. To get encryption you
layer **TLS 1.3** on top, which adds a separate handshake. In this project TCP is
`tls.Dial`/`tls.Listen`. Its strengths show on LAN (cheap kernel/hardware data path) and at
extreme broadcast fan-out (kernel socket writes cheaper than userspace QUIC stream writes).

## User space vs kernel space (the central explanatory axis)
- **Kernel space:** privileged OS core. TCP's state machine, retransmission, and congestion
  control run here, and the data path can be **offloaded to NIC hardware** (see TSO/GRO/GSO).
  Moving bytes costs few CPU cycles per byte.
- **User space:** ordinary application memory/CPU. **quic-go implements QUIC entirely in user
  space** on top of a UDP socket. Every QUIC packet is built, encrypted, congestion-checked,
  and handed to the kernel via a syscall **by the application itself**.
- **Why it matters to the results:** the decisive variable in this study is **latency-bound
  vs CPU-bound**.
  - *Latency-bound (WAN):* RTT (~90 ms) dwarfs per-packet CPU cost, so QUIC's structural
    round-trip savings dominate → QUIC wins.
  - *CPU-bound (LAN/loopback):* RTT ≈ 0, so the userspace per-packet cost is fully exposed
    while TCP rides kernel/hardware offload → TCP wins (and the gap widens with payload
    size). This is the **LAN/WAN crossover** (KB 04).

## GSO — Generic Segmentation Offload
A Linux optimisation that lets an application hand the kernel **one large buffer**; the
kernel (or NIC) splits it into many MTU-sized packets just before transmission. For UDP/QUIC
this is huge: instead of one `sendmsg` syscall per ~1200-byte datagram, quic-go can pass a
big coalesced buffer in one syscall, amortising syscall and per-packet overhead.
- **Why it matters here:** GSO is what narrows QUIC's userspace CPU penalty on Linux. With
  GSO off, QUIC must syscall every small datagram individually — slow.
- **The catch (the bug we hit):** some WAN paths (cross-region cloud overlays, certain
  middleboxes) **silently drop GSO-coalesced UDP datagrams**. The QUIC *handshake* still
  succeeds (small single packets) but sustained data transfer got **0% delivery**. Fix: set
  `QUIC_GO_DISABLE_GSO=true` (the `--disable-gso` flag) so quic-go sends plain un-coalesced
  datagrams over WAN. Trade-off: more syscalls / higher CPU, but it actually works.

## ECN — Explicit Congestion Notification
A 2-bit field in the IP header that lets routers **mark** packets to signal congestion
*instead of dropping them*; the transport reacts by slowing down. QUIC can use ECN for
smarter congestion control.
- **The catch (same class of bug as GSO):** some paths **drop or mangle ECN-marked UDP
  datagrams**, again breaking QUIC data transfer over that path. Fix:
  `QUIC_GO_DISABLE_ECN=true` (`--disable-ecn`). Disabled by default in the relay and load
  tester for WAN robustness.
- These two (`QUIC_GO_DISABLE_GSO` / `QUIC_GO_DISABLE_ECN`) are environment variables read by
  quic-go **at socket-creation time**, which is why `main.go` / `loadtest/main.go` set them
  via `os.Setenv` *before* any QUIC socket is created.

## TSO / GRO — TCP Segmentation Offload / Generic Receive Offload
The TCP-side equivalents of GSO, done in NIC hardware: TSO lets the NIC split a large TCP
send buffer into segments; GRO coalesces received segments before handing them up. **This is
the hardware advantage TCP enjoys that userspace QUIC cannot match on a fast/clean link** —
the core reason TCP wins bulk transfers on LAN.

## RTT — Round-Trip Time
Time for a packet to go to the peer and a response to come back. The dominant cost over WAN.
In the test bed base RTT ≈ **90 ms** (EU↔US). Almost every "tie ≤4KB" result is because a
single message just rides one RTT regardless of protocol. Measured in the load tester as
`time.Since(sendTime).Milliseconds()` keyed by msgID.

## Head-of-Line (HOL) blocking
When one lost/delayed packet stalls *everything* queued behind it.
- **TCP:** a single byte-stream — one lost segment blocks all data after it (and, for N
  parallel logical channels multiplexed on one TCP connection, blocks all of them).
- **QUIC:** independent streams — loss on stream *i* does **not** stall streams *j≠i*. This
  is the structural reason QUIC wins dimension 3 (multistream), especially under loss.

## Star topology / relay
Instead of a full mesh (every peer connected to every other — O(n²) connections), this app
uses a **relay**: one hub (`--relay`) that every peer connects to once; the hub re-broadcasts
each message to the rest. O(n) connections, and it solves NAT traversal (peers behind NAT
can't accept inbound connections, but they *can* dial out to a public relay). Cost: the relay
sees plaintext (not E2E) and is a fan-out bottleneck at extreme scale (dimension 4 tail).

## TLS 1.3 / transport encryption vs end-to-end (E2E) encryption — BE PRECISE
- **Transport-level encryption (what this app has):** every hop is TLS 1.3 encrypted. A
  network eavesdropper sees only ciphertext.
- **NOT end-to-end:** because messages go **through the relay**, and **the relay terminates
  TLS**, the relay decrypts each message to plaintext before re-encrypting and re-broadcasting.
  So the relay operator *could* read messages. True E2E (where only sender and recipient hold
  keys, relay forwards ciphertext) is **future work**, explicitly not implemented.
- **Why this surfaced late:** the whole study is about transport *performance*; nothing in the
  benchmark touches the encryption *model*, so it never came up until the dissertation needed
  to state the security scope accurately. On LAN (direct peer-to-peer, no relay) the hop is
  still only TLS — it being "direct" doesn't make it E2E in any cryptographic sense.

## TOFU — Trust On First Use
Because certs are self-signed (no CA / PKI), the client **pins** each peer's certificate
SHA-256 fingerprint on first contact (`VerifyPeerCertificate` in `connection_manager.go`). A
later connection presenting a *different* fingerprint for the same address is rejected as a
possible man-in-the-middle. Same trust model SSH uses. Limitation: the very first connection
is trusted blindly.

## argon2id — password-based room key derivation
Rooms can be password protected. The password is stretched with **argon2id**
(`argon2.IDKey`, params: time=1, memory=64 MiB, parallelism=4, 32-byte output, salt = room
name) in `security.go`. argon2id is a memory-hard KDF (Password Hashing Competition winner,
2015) — memory-hardness makes brute-force/GPU attacks expensive. The derived key is compared
in **constant time** (`subtle.ConstantTimeCompare`) to avoid timing side-channels. **Note:**
this authenticates room *entry*; it is not message-content encryption and not E2E.

## STUN / UPnP / NAT-PMP — getting reachable over the internet
- **STUN** (`/myip`): asks a public server "what does my public IP:port look like?" — used to
  advertise a reachable address.
- **UPnP IGD / NAT-PMP** (`upnp.go`): ask the home router to open/forward a port so a relay
  behind NAT is reachable from the WAN. `--no-upnp` disables (used on cloud VMs that already
  have public IPs).

## quic-go library specifics worth knowing
- Concurrent writes to a single QUIC **stream** are unsafe → the project serialises all writes
  to a connection through one `writeLoop` goroutine under a mutex.
- Flow-control windows are tunable (`performance.go`): bigger windows let more data be
  in-flight before waiting for an ACK — important for large-payload throughput over high-RTT
  links (dimension 2 large-message win).
- quic-go needs **large UDP socket buffers**; if `rmem_max`/`wmem_max` are small the kernel
  drops datagrams under load (`CheckUDPBuffers` warns; `scripts/apply_sysctl.sh` /
  `raw_mini/tune_udp.sh` raise them).
