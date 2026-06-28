# Knowledge Base — Index (read this first)

> **Purpose.** This folder is the persistent memory for the AI assistant working on this
> project. Conversations hit their context limit and get reset; this KB exists so a fresh
> session can get fully up to speed *without* re-reading the whole codebase. Read these
> files at the start of any new session before doing real work.

## What this project is (one paragraph)

A secure, decentralised peer-to-peer (P2P) command-line messaging app written in **Go**,
built to answer one research question: **for small-message P2P chat, does QUIC outperform
TCP, on which metrics, and at what scale does either protocol break?** The same app speaks
**both** QUIC and TCP (selected with a flag) so the two transports can be compared on
identical application logic. It has LAN auto-discovery (UDP broadcast), an optional
**relay** mode (star topology hub for wide-area use), password-protected rooms
(argon2id), and TOFU certificate pinning over TLS 1.3. A separate load-testing harness
(`loadtest/`) drives a 5-dimension QUIC-vs-TCP benchmark; `raw_mini/` holds lower-level
raw-transport micro-benchmarks.

## Read order for a new session

| # | File | What it gives you |
|---|---|---|
| 01 | `01-codebase-map.md` | Every Go file and what it does; data flow; key types |
| 02 | `02-benchmark-methodology.md` | The 5 dimensions, exact params/bytes, how each metric is measured |
| 03 | `03-technical-concepts.md` | Glossary: GSO, ECN, user vs kernel space, TSO/GRO, RTT, HOL blocking, 0-RTT, TOFU, argon2id |
| 04 | `04-testbed-and-results.md` | WAN/LAN test beds, the exact result numbers, the LAN/WAN crossover |
| 05 | `05-project-timeline.md` | Project history reconstructed from git |
| 06 | `06-dissertation-status.md` | Report structure, what's written, hard rules (academic integrity, security accuracy, build pipeline) |

## Hard rules that override everything (summary — full text in 06)

1. **Academic integrity.** The user writes the actual dissertation prose. The AI provides
   scaffolds, grounded technical facts, citations, grammar/spelling fixes — but does
   **not** ghost-write submittable prose.
2. **Grammar fixes only fix what's broken.** Preserve the user's wording and voice; do not
   rewrite.
3. **Security accuracy.** Messages are **transport-level TLS 1.3 encrypted, NOT
   end-to-end**. The relay terminates TLS and sees plaintext before re-broadcasting. Never
   claim E2E.
4. **The contaminated commit `38bba13`.** Its commit *message* mentions Double Ratchet,
   C++, Dart, acoustic handshake, APK signing — those belong to a *different* project and
   do **not** exist in this Go codebase. Its actual code changes (password rooms /
   argon2id) are real. Never cite Double Ratchet / acoustic / C++ / Dart anywhere.
5. **"2× faster" applies to connection setup only**, not QUIC overall.
6. **Build pipeline.** Never hand-edit `report/dissertation.tex` (generated). Edit markdown
   then run `report/build.sh`. Avoid Unicode arrows in markdown (`->` not the arrow glyph).

## Where the source of truth lives (don't duplicate, reference)

- Full results + methodology narrative: `docs/quic-vs-tcp-study-20260602.md`
- The root-cause debug write-up (relay collapse + GSO/ECN): `docs/debug-benchmark-results-20260601.md`
- Adaptive transport design + LAN/WAN crossover: `docs/adaptive-transport-analysis.md`
- Sub-agent exploration notes: `docs/subagent-docs/`
- Dissertation chapters: `report/chapters/*.md`
