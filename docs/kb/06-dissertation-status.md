# Dissertation Status, Structure, and Hard Rules

The dissertation lives in `report/` (markdown -> LaTeX -> PDF). The writing plan is
`docs/dissertation-outline.md` (section order, word budget, what each section says, which
artefact backs it). This file tracks *status* and the *non-negotiable rules*.

## Build pipeline (exact)
- Edit `report/chapters/*.md`, `report/abstract.md`, `report/acknowledgements.md` (plain
  Markdown).
- `./report/build.sh` converts markdown -> `report/dissertation.tex` (pandoc).
- `tectonic report/dissertation.tex` -> `report/dissertation.pdf` (self-contained, no
  system LaTeX). Tools installed via brew: `pandoc`, `tectonic`, mermaid via `npx`.
- **NEVER hand-edit `report/dissertation.tex`** — it is overwritten every build.
- **NEVER hand-edit `report/template.tex`** except the frontmatter (title/author/declaration).
  It is faithful to the CITY College / University of York UG template.
- **Use empty stdin with tectonic** (`echo "" | tectonic ...`) to avoid interactive hangs.
- **Avoid Unicode glyphs in markdown** that break LaTeX — use `->` not the arrow glyph; the
  ✅/➖ in KB files are fine here but should not go into chapter markdown.
- Known tooling quirk: the `fs_write` editor tool has intermittently failed to persist
  `06-results.md` (landed empty twice). Workaround: write that file via bash
  (`printf`/Python heredoc) instead of the editor tool.

## Chapter status (as of 2026-06-05)

| File | Chapter | Status |
|---|---|---|
| `abstract.md` | Abstract | drafted |
| `01-introduction.md` | Introduction | **DONE** — user wrote prose; AI did grammar-only + structure. Has Aim/Objectives, Contributions, Report Structure. |
| `02-background.md` | Background & Related Work | **DONE** — user prose; AI grammar fixes + converted all `[CITE:]` to numbered `[1]`–`[19]`; refs verified; IEEE list in `10-references.md`. |
| `03-design.md` | Requirements & Design | scaffold + 6 embedded Mermaid figures w/ numbered captions; **user prose pending** |
| `04-implementation.md` | Implementation | scaffold; pending |
| `05-methodology.md` | Evaluation Methodology | scaffold; pending |
| `06-results.md` | Results & Analysis | scaffold (the empty-write-quirk file); pending — THE CORE chapter |
| `07-discussion.md` | Discussion (+ adaptive transport) | scaffold; pending |
| `08-reflection.md` | Reflection | stub (9 lines); pending |
| `09-conclusion.md` | Conclusion & Future Work | stub; pending |
| `10-references.md` | References | IEEE list `[1]`–`[19]`, verified online |
| `11-appendix.md` | Appendix | reproduction commands / data; partial |

Figures: 6 Mermaid diagrams in `report/diagrams/*.mmd` rendered to `report/figures/0*.png`
(`bash report/diagrams/render.sh`): 01-architecture, 02-startup-discovery,
03-connection-handshake, 04-message-broadcast, 05-healthcheck, 06-topologies. Result charts
in `bench_charts/compare/*.png`. `RELATED_WORK_SOURCES.md` tracks reference provenance.

## Current directive (post-extension)
The user got a **1-week extension and dropped the 10k-word limit** — the dissertation now
needs **much more depth**: explain GSO/ECN, user-space vs kernel-space, HOL blocking, the
"why this, why that" of every design decision, limitations, etc. The `dissertation-outline.md`
word budget (10k) is now a *floor for structure*, not a ceiling. Expand each chapter with the
mechanism-level explanations captured in KB 03. KB 02/03/04 exist precisely to feed this depth.

## Reference list (verified) — quick map
`10-references.md` holds `[1]`–`[19]` in IEEE style. Bucket summary (open each yourself before
the viva): [1] TCP / 3-way handshake; [2] TLS 1.3 (RFC 8446); [3] TSO/GRO kernel offload;
[4] QUIC (RFC 9000); [5]/[6] QUIC TLS + loss detection (RFC 9001/9002); [7] quic-go; [8] NAT;
[9] UPnP/NAT-PMP; [10] STUN; [11]/[12] ICE/TURN; [13] TOFU; [14] argon2; [15]–[18] QUIC-vs-TCP
measurement studies (incl. "QUIC slower on fast nets" userspace-cost finding); [19] large-scale
P2P NAT connectivity measurement. **Rule: every source must be one the user has actually
opened/verified.** Never fabricate.

## HARD RULES (these override task-completion urgency)

1. **Academic integrity — the user writes the prose.** The AI provides: scaffolds, grounded
   technical facts (from code/results), citations (verified), grammar/spelling fixes, and
   structure. The AI does **not** ghost-write submittable prose. When the user is stuck, coach
   and supply facts/bullets — do not hand them finished paragraphs to submit as their own.
   (The user once added a "no AI assistance" declaration clause "by accident"; regardless,
   keep authorship genuinely theirs.)

2. **Grammar fixes fix only what is broken.** Preserve the user's wording, voice, and
   structure. Do not rewrite sentences that are merely not how you'd phrase them.

3. **Security accuracy (examiners WILL probe this).** Messages are **transport-level TLS 1.3
   encrypted, NOT end-to-end**. The relay terminates TLS and sees plaintext before
   re-broadcasting. LAN-direct is still only transport TLS, not E2E. True E2E = future work.

4. **The contaminated commit `38bba13`.** Its message mentions Double Ratchet, C++ crypto,
   Dart, acoustic handshake, ASan/UBSan, APK signature verification — **all from a DIFFERENT
   project; none exist in this Go codebase.** Its real contribution here is password rooms /
   argon2id. Leave the commit untouched. **Never** mention Double Ratchet / acoustic / C++ /
   Dart / Android anywhere in the dissertation.

5. **Don't overstate.** "QUIC ~2× faster" applies to **connection setup only**. QUIC ties on
   small-message latency and loses bulk on LAN. Be precise per-dimension.

6. **Verify, don't fabricate.** Numbers come from the CSVs / study docs (KB 04). Citations are
   verified online and must be openable by the user. If unsure, say so.

7. **Separate measured from proposed.** The adaptive transport is *designed, not built* — always
   label it as such. Examiners reward this honesty.

## Threats to validity to keep visible (for §6.6/§8.4)
Burstable t3 instances (CPU credits can skew sustained runs); macOS limited GSO inflates the
LAN QUIC penalty; single relay (no multi-relay/sharding tested); RTT is relay-mediated (peer→
relay→peer), not pure point-to-point transport; small latency-run sample (n=10, 1 msg each);
0-RTT replay-safety caveat; LAN measured on loopback (no real NIC/switch).
