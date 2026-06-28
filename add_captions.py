#!/usr/bin/env python3
"""Add captions to the 8 longtables in dissertation.tex."""

with open("report/dissertation.tex", "r") as f:
    content = f.read()

lines = content.split("\n")

# Define the 8 tables in order of appearance.
# Each entry: (caption_text, label, first_header_text) to identify the table.
tables = [
    {
        "caption": "Application components and their responsibilities, including the file or package that owns each one and what it does at a high level.",
        "label": "tab:components",
    },
    {
        "caption": "Application message protocol fields exchanged between peers over the encrypted connection (newline-delimited UTF-8 text).",
        "label": "tab:protocol",
    },
    {
        "caption": "WAN test bed configuration: a relay in EU (Frankfurt) and a load generator in US (Virginia) on AWS t3.large, connected via cross-region link with approximately 90 ms base RTT.",
        "label": "tab:testbed",
    },
    {
        "caption": "The five benchmark dimensions evaluated in this study, with the metric each measures and its purpose.",
        "label": "tab:dimensions",
    },
    {
        "caption": "Research hypotheses and the expected winner for each, based on the known properties of the two transports.",
        "label": "tab:hypotheses",
    },
    {
        "caption": "Chapter at a Glance: 10 result sections, their figures, and the one-line headline finding for each.",
        "label": "tab:glance",
    },
    {
        "caption": "Effect of the broadcast-path optimisation on n=1000 broadcast RTT p50: baseline (old code) vs improved (new code) for both protocols.",
        "label": "tab:bcast-opt",
    },
    {
        "caption": "0-RTT reconnection measured on real WAN with the corrected 4-metric methodology: local dial return, handshake complete, first app-data sent, and first response byte. 1-RTT and 0-RTT run for 10 connections each, with warm-up, reported as mean ± 95% CI.",
        "label": "tab:0rtt",
    },
]

# Find the closing line of each longtable's column spec (the line containing "@{}}").
# We need to find the first 8 @{}} lines that belong to a longtable start.
# A longtable that already has a caption (the 9th one) will be skipped.

colspec_close_lines = []
in_longtable_start = False
for i, line in enumerate(lines):
    if line.strip().startswith(r"\begin{longtable}"):
        in_longtable_start = True
        # If the @{}} is on the same line as \begin{longtable
        if "@{}}" in line:
            colspec_close_lines.append(i)
            in_longtable_start = False
    elif in_longtable_start and "@{}}" in line:
        colspec_close_lines.append(i)
        in_longtable_start = False

print(f"Found {len(colspec_close_lines)} longtable column-spec closing lines")
for idx in colspec_close_lines:
    print(f"  Line {idx + 1}: {lines[idx][:100]}...")

# We need to skip the 9th one which already has a caption.
# The 9th longtable is at line ~1075. Let's check which ones already have a caption.
# Actually, the 9th one already has a caption. We should only insert for the first 8.
# But let's verify by checking if the line after the colspec closing already has \caption.

# Filter: skip any that already have a \caption right after.
target_lines = []
for idx in colspec_close_lines:
    # Check the next line (or two) for \caption
    next_line = lines[idx + 1] if idx + 1 < len(lines) else ""
    next_next = lines[idx + 2] if idx + 2 < len(lines) else ""
    if r"\caption" in next_line or r"\caption" in next_next:
        print(f"  SKIP line {idx + 1}: already has caption")
        continue
    target_lines.append(idx)

print(f"\nTarget lines for caption insertion: {len(target_lines)}")
for idx in target_lines:
    print(f"  Line {idx + 1}")

assert len(target_lines) >= 8, (
    f"Expected at least 8 tables without captions, found {len(target_lines)}"
)
# Take only first 8 if there are more
target_lines = target_lines[:8]

# Process in reverse order to keep line numbers stable
for i in reversed(range(len(tables))):
    t = tables[i]
    insert_at_line = target_lines[i]
    caption_line = f"\\caption{{{t['caption']}}}\\label{{{t['label']}}} \\\\"
    print(f"\nTable {i + 1} ({t['label']}): inserting after line {insert_at_line + 1}")
    print(f"  Caption: {caption_line[:90]}...")

    # Insert after the colspec closing line
    lines.insert(insert_at_line + 1, caption_line)

# Write back
with open("report/dissertation.tex", "w") as f:
    f.write("\n".join(lines))

print("\nDone. Updated dissertation.tex with 8 captions.")
