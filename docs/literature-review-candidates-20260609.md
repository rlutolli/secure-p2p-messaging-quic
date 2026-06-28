# Literature-Review Candidate Sources (from deep-research brief, 2026-06-09)

> **STATUS: UNVERIFIED — verify every entry before citing.** These came from an external
> deep-research tool. Most are real and well-known, but a few citations/numbers need
> confirming (flagged ⚠). Open each source yourself, confirm venue/year/numbers, and only
> then add to `report/chapters/10-references.md`. Do not cite anything you have not opened.
> Maps directly onto the expanded outline's Background (Ch2), Results (Ch6), Discussion (Ch7).

## How it corroborates our 5 findings (one-liners)
1. **QUIC ~2× connect** — Langley et al., SIGCOMM 2017 (Google deployment, 88% 0-RTT success).
2. **Small-msg WAN parity** — TCP ~0.3 ms faster CPU-side on clean LAN, masked by 90 ms RTT.
3. **QUIC loses on clean LAN (CPU)** — "QUIC is not Quick Enough over Fast Internet" (IMC 2023):
   >600 Mbps → QUIC up to 45% lower; receiver processing RTT 16.2 ms vs HTTP/2 1.9 ms; root
   cause = UDP GRO can't offload variable-size encrypted QUIC frames.
4. **QUIC multistream/loss win** — HOL-blocking elimination + RFC 9002 monotonic packet numbers
   (no RTO ambiguity); our 3% loss tail data matches this exactly.
5. **TCP cheaper at extreme fan-out** — userspace QUIC ~50% more syscalls / ~63% more CPU cycles
   (kQUIC study ⚠); SFUs cap ~500–2000 streams before cascading.

## A. Measurement studies (Ch2 / Ch6)
- **Langley et al., "The QUIC Transport Protocol: Design and Internet-Scale Deployment," ACM
  SIGCOMM 2017.** Real — the foundational QUIC paper. 88% 0-RTT. Use for Findings 1.
- **Zhang et al., "QUIC is not Quick Enough over Fast Internet," ACM IMC 2023.** Real, important.
  Userspace receive-side processing as the bottleneck on fast links; GRO offload gap. Finding 3.
- ⚠ "Performance Evaluation of QUIC for Cloud Control Systems," IEEE LCN — verify exact venue;
  the "TCP ~0.3 ms faster small-msg on clean LAN" claim. Finding 2.
- ⚠ MQTT-over-QUIC IoT testbed (IJECE/IEEE Access 2024) — verify; supports small-msg reliability
  on wireless. Finding 2.

## B. Userspace vs kernel cost (Ch2 §2.6/2.7, Ch6 fan-out)
- ⚠ **kQUIC (Kumar & Dezfouli, IEEE Access 2025)** — "50% more syscalls, 63% more CPU cycles" vs
  TCP for 1000–1400 B userspace QUIC; +26% copy overhead at 2.5% loss. Verify it exists/numbers.
- **Fastly eng blog (2020), "measuring QUIC vs TCP efficiency"** — GSO coalescing 10 pkts: 240→348
  Mbps (+45%); 1280→1460 B → 464 Mbps. Real-ish; confirm URL. Finding 3/5.
- **Cloudflare eng blog (2020), "Accelerating UDP packet transmission for QUIC"** — `sendmmsg` +
  `UDP_SEGMENT` (Linux 4.18+): 640 Mbps → 1.6 Gbps. Real. Motivates our coalesced-write change.

## C. Packet-loss recovery (Ch2, Ch6 loss section)
- **RFC 9002** (QUIC loss detection & CC) — primary source; monotonic packet numbers, no RTO
  ambiguity. Cite directly.
- ⚠ "QUIC 4.6× higher throughput than TCP at 20% loss" (CDNetworks / tunneling study) — verify.
- ⚠ "quicSDN," J. Network & Computer Applications 2023 — −82% overhead, −45% delay in SDN. Verify.

## D. P2P / relay transport choices & scaling limits (Ch2 §2.8, Ch7)
- **libp2p** — transport-agnostic, parallel-dials TCP+QUIC; Circuit Relay v2 (Hop/Stop);
  fan-out bounded by file-descriptor limits (~1000) without resource-manager scopes. (Mirrors
  our n=1000 ceiling + dual-stack rationale.) Cite libp2p docs/specs.
- **Matrix/Pinecone** — SNEK routing P2P overlay; favours TCP/WebSockets for firewall traversal.
- **Signal** — WebRTC SFU (LiveKit) for calls; group messaging "fan-out encryption" with a hard
  **100-member group cap** to avoid fan-out explosion. (Useful scaling-limit precedent.)
  NOTE: cite Signal's *published* design only; nothing here relates to our codebase.
- **Tailscale/DERP** — UDP hole-punch first, fallback to encrypted relay over HTTPS/QUIC; added
  TUN UDP GSO/GRO to wireguard-go for ~4× throughput. (Direct precedent for our relay fallback.)
- **WebRTC/MoQ** — SFUs O(N) per client, ~500–2000 streams/node before cascading; industry moving
  to Media-over-QUIC relays. (Supports Finding 5 fan-out ceiling.)

## E. Adaptive transport / connection racing (Ch7 adaptive design)
- **RFC 8305 Happy Eyeballs** — race connections; primary source.
- Heuristics in the wild (verify each): curl QUIC→TCP after 100 ms (no UDP) / 200 ms; Chrome
  delays TCP up to 300 ms when Alt-Svc advertises QUIC, falls back H3→H2 if TTFB >~400 ms;
  Facebook 200 ms QUIC→TCP; MoQT 100 ms. UDP blocked on ~3–5% of networks (motivates fallback).
- **ALPN** (in RFC 8446) — negotiates h3 etc. during handshake.

## F. Standards to cite precisely (Ch2, Ch10)
RFC 9000 (QUIC transport), RFC 9001 (QUIC+TLS), RFC 9002 (QUIC loss/CC), RFC 8446 (TLS 1.3),
RFC 9293 (TCP), RFC 3168 (ECN), RFC 8305 (Happy Eyeballs), RFC 8445 (ICE), RFC 5389 (STUN),
RFC 8656 (TURN), RFC 9106 (argon2). All real; cite as primary sources.

## TCP-vs-QUIC architecture comparison table (for Ch2 — verify framing, then reuse)
Transport foundation (kernel/IP vs userspace/UDP); setup (2–3 RTT vs 1-RTT/0-RTT); security
(separate TLS vs integrated TLS 1.3); multiplexing (single stream vs independent streams); HOL
blocking (yes vs eliminated per-stream); loss detection (seq+RTO ambiguity vs monotonic packet
numbers); hardware offload (mature TSO/GRO/LRO vs limited UDP GSO/GRO); CPU on clean LAN
(efficient vs high userspace overhead); migration (drops on IP/port change vs Connection ID).
