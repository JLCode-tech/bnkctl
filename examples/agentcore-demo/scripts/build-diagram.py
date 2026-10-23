#!/usr/bin/env python3
"""Render the demo's diagrams as SVGs, for slides.

  python3 scripts/build-diagram.py     -> images/three-paths.svg
                                       -> images/estate-token-governance.svg
                                       -> images/network-and-paths.svg

No dependencies. Convert to PNG with:
  rsvg-convert -w 2400 images/three-paths.svg -o images/three-paths.png
"""

import argparse
import pathlib
import xml.sax.saxutils as xml

W, H = 1180, 760
INK = "#1c2530"
MUTED = "#6b7785"
LINE = "#9aa5b1"
F5 = "#e4002b"
AWS = "#ff9900"
OK = "#127c3f"
DENY = "#b4232c"
BG = "#ffffff"
PANEL = "#f6f8fa"

out = []


def esc(t):
    return xml.escape(str(t))


def text(x, y, s, size=13, fill=INK, weight="normal", anchor="start", mono=False):
    fam = ("ui-monospace,SFMono-Regular,Menlo,monospace" if mono
           else "-apple-system,Segoe UI,Inter,Helvetica,Arial,sans-serif")
    out.append(
        f'<text x="{x}" y="{y}" font-family="{fam}" font-size="{size}" fill="{fill}" '
        f'font-weight="{weight}" text-anchor="{anchor}">{esc(s)}</text>'
    )


def box(x, y, w, h, label, sub=None, stroke=LINE, fill="#fff", dash=None, lw=1.4):
    d = f' stroke-dasharray="{dash}"' if dash else ""
    out.append(
        f'<rect x="{x}" y="{y}" width="{w}" height="{h}" rx="7" fill="{fill}" '
        f'stroke="{stroke}" stroke-width="{lw}"{d}/>'
    )
    cy = y + h / 2 + (0 if sub is None else -7)
    text(x + w / 2, cy + 4, label, 12.5, INK, "600", "middle")
    if sub:
        text(x + w / 2, cy + 21, sub, 10.5, MUTED, "normal", "middle", mono=True)


def frame(x, y, w, h, stroke=LINE, fill="#fff", dash=None, lw=1.4):
    d = f' stroke-dasharray="{dash}"' if dash else ""
    out.append(
        f'<rect x="{x}" y="{y}" width="{w}" height="{h}" rx="7" fill="{fill}" '
        f'stroke="{stroke}" stroke-width="{lw}"{d}/>'
    )


def arrow(x1, y1, x2, y2, label=None, colour=LINE, dash=None, above=True):
    d = f' stroke-dasharray="{dash}"' if dash else ""
    out.append(
        f'<line x1="{x1}" y1="{y1}" x2="{x2}" y2="{y2}" stroke="{colour}" '
        f'stroke-width="1.6" marker-end="url(#a)"{d}/>'
    )
    if label:
        mx, my = (x1 + x2) / 2, (y1 + y2) / 2
        text(mx, my - 7 if above else my + 15, label, 10, MUTED, anchor="middle", mono=True)


def panel(x, y, w, h, n, title, status, colour):
    out.append(
        f'<rect x="{x}" y="{y}" width="{w}" height="{h}" rx="10" fill="{PANEL}" '
        f'stroke="#e3e8ee" stroke-width="1"/>'
    )
    out.append(f'<circle cx="{x+22}" cy="{y+24}" r="11" fill="{colour}"/>')
    text(x + 22, y + 28, n, 12, "#fff", "700", "middle")
    text(x + 42, y + 29, title, 14, INK, "700")
    text(x + w - 14, y + 29, status, 11, MUTED, "600", "end")


def build():
    out.clear()
    out.append(
        f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {W} {H}" width="{W}" height="{H}">'
    )
    out.append(
        f'<defs><marker id="a" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" '
        f'markerHeight="7" orient="auto-start-reverse">'
        f'<path d="M 0 0 L 10 5 L 0 10 z" fill="{LINE}"/></marker></defs>'
    )
    out.append(f'<rect width="{W}" height="{H}" fill="{BG}"/>')

    text(40, 44, "Governing the agentic action path", 23, INK, "700")
    text(40, 68, "Amazon Bedrock AgentCore  ×  F5 BIG-IP Next for Kubernetes", 13, MUTED)
    text(W - 40, 68, "the tool hop is the one hop you control", 11.5, F5, "600", "end")

    # ── Path 1 ───────────────────────────────────────────────────────────────
    py = 96
    panel(40, py, W - 80, 178, "1", "Trusted agent path", "runs today", OK)
    box(70, py + 52, 168, 62, "AgentCore Runtime", "ENIs .11.15 / .12.191", AWS)
    text(70, py + 130, "multi-homed — key limits on", 9.5, MUTED)
    text(70, py + 144, "caller identity, not client IP", 9.5, MUTED)
    box(302, py + 34, 150, 52, "Amazon Bedrock", "stopReason: tool_use", AWS)
    arrow(238, py + 62, 302, py + 50, "1. reason", AWS)
    arrow(302, py + 74, 240, py + 80, "4. narrate", AWS, above=False)
    frame(520, py + 40, 210, 104, F5, "#fff", None, 2)
    text(625, py + 64, "F5 BNK  (TMM)", 12.5, INK, "600", "middle")
    text(625, py + 84, "VIP 10.0.10.150", 10.5, MUTED, anchor="middle", mono=True)
    text(625, py + 102, "authz · rate · firewall · log", 10, MUTED, anchor="middle", mono=True)
    text(625, py + 126, "ONLY CHECKPOINT", 9.5, F5, "700", "middle")
    arrow(238, py + 100, 520, py + 100, "2. tools/call", LINE, above=False)
    box(792, py + 62, 150, 58, "MCP tool pod", "forecast()", LINE)
    arrow(730, py + 92, 792, py + 92, "3. fact", OK)
    text(1000, py + 84, "AgentCore is not in", 10.5, MUTED)
    text(1000, py + 99, "the tool hop, by design.", 10.5, MUTED)

    # ── Path 2 ───────────────────────────────────────────────────────────────
    py = 296
    panel(40, py, W - 80, 168, "2", "Double-checked path", "not built", MUTED)
    box(70, py + 54, 168, 58, "AgentCore Runtime", None, AWS)
    box(292, py + 44, 186, 78, "AgentCore Gateway", "Cedar · guardrails", AWS, dash="5 4")
    text(385, py + 108, "identity-aware per-tool authz", 9.5, MUTED, anchor="middle", mono=True)
    arrow(238, py + 82, 292, py + 82, "MCP", LINE)
    box(532, py + 52, 176, 62, "VPC Lattice", "resource gateway ENIs", AWS, dash="5 4")
    arrow(478, py + 82, 532, py + 82)
    box(762, py + 44, 186, 78, "F5 BNK  (TMM)", "network + workload policy", F5, dash="5 4", lw=2)
    arrow(708, py + 82, 762, py + 82)
    box(998, py + 54, 132, 58, "MCP tool pod", None, LINE, dash="5 4")
    arrow(948, py + 82, 998, py + 82)
    text(70, py + 140, "Two independent policy engines, different questions. Blocked on: no Gateway deployed, "
                        "and a publicly trusted cert for the target.", 10.5, MUTED)

    # ── Path 3 ───────────────────────────────────────────────────────────────
    py = 486
    panel(40, py, W - 80, 200, "3", "Stranger path", "runs today", OK)
    box(70, py + 62, 176, 62, "Unmanaged caller", "another cloud · a script", MUTED)
    frame(330, py + 34, 250, 130, F5, "#fff", None, 2)
    text(455, py + 52, "F5 BNK  (TMM)", 12.5, INK, "600", "middle")
    text(455, py + 68, "refused before the pod", 9.5, MUTED, anchor="middle", mono=True)
    for i, (line, col) in enumerate([
        ("403  privileged tool", DENY),
        ("429  rate limit exceeded", DENY),
        ("---  non-VPC source rejected", DENY),
        ("200  forecast → forwarded", OK),
    ]):
        text(348, py + 90 + i * 17, line, 10.5, col, "600", mono=True)
    arrow(246, py + 99, 330, py + 99, "POST", LINE)
    box(646, py + 70, 150, 58, "MCP tool pod", None, LINE)
    arrow(580, py + 99, 646, py + 99)
    text(646, py + 148, "401  no / wrong credential", 10.5, DENY, "600", mono=True)
    text(646, py + 163, "issued by the tool, not BNK", 9.5, MUTED)
    text(830, py + 74, "No AgentCore component is in", 11, INK, "600")
    text(830, py + 91, "this path — no JWT check, no", 10.5, MUTED)
    text(830, py + 106, "Cedar, no guardrail. This is the", 10.5, MUTED)
    text(830, py + 121, "case a managed front door", 10.5, MUTED)
    text(830, py + 136, "cannot cover.", 10.5, MUTED)

    text(40, H - 16, "Verified live on bnk-agentcore-demo, ap-southeast-2. "
                     "Dashed = not yet built.", 10, MUTED)
    out.append("</svg>")
    return "\n".join(out)




def build_estate():
    """BNK enforces in-path; Forge accounts for everything AWS logs."""
    out.clear()
    W2, H2 = 1220, 720
    out.append(
        f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {W2} {H2}" width="{W2}" height="{H2}">'
    )
    out.append(
        f'<defs><marker id="a" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" '
        f'markerHeight="7" orient="auto-start-reverse">'
        f'<path d="M 0 0 L 10 5 L 0 10 z" fill="{LINE}"/></marker></defs>'
    )
    out.append(f'<rect width="{W2}" height="{H2}" fill="{BG}"/>')

    text(40, 44, "Token governance across the AWS AI estate", 23, INK, "700")
    text(40, 68, "BNK enforces where it is in the path. Forge accounts for everything AWS logs. "
                 "Together they cover the estate.", 13, MUTED)

    # (label, AWS TPM covers it, BNK can be in path, AWS writes CloudWatch logs)
    rows = [
        ("Agent via AgentCore Gateway", True, True, True),
        ("Agents on EKS / ECS / EC2", False, True, True),
        ("Self-hosted vLLM / NIM on GPU nodes", False, True, False),
        ("Third-party LLM APIs (OpenAI, ...)", False, True, False),
        ("SageMaker endpoints", False, True, True),
        ("Apps calling Bedrock over the AWS backbone", False, False, True),
    ]
    text(40, 112, "WHERE THE TOKENS ARE BURNED", 10.5, MUTED, "700")
    y0 = 130
    for i, (label, tpm, inpath, cwlogs) in enumerate(rows):
        y = y0 + i * 44
        stroke = LINE if inpath else "#d8dee5"
        fill = "#fff" if inpath else "#f4f6f8"
        frame(40, y, 322, 34, stroke, fill, None if inpath else "4 3")
        text(52, y + 22, label, 11, INK if inpath else MUTED, "600")
        if tpm:
            frame(370, y + 5, 84, 24, AWS, "#fff8ee", None, 1.2)
            text(412, y + 21, "RPM·TPM·CPS", 8, "#8a5200", "700", "middle")
            text(412, y + 40, "AWS covers this row", 8.5, MUTED, anchor="middle")
        if inpath:
            arrow(462, y + 17, 543, 208, None, F5)
        elif cwlogs:
            arrow(462, y + 17, 543, 372, None, "#c9d2db", "3 3")

    # lane A: BNK, in-path enforcement
    frame(545, 122, 232, 172, F5, "#fff", None, 2)
    text(661, 148, "F5 BNK", 14, INK, "700", "middle")
    text(661, 165, "in path \u2014 CAN STOP IT", 9.5, F5, "700", "middle")
    for i, line in enumerate([
        "per-user / per-model limits",
        "429 when budget is spent",
        "counts in dSSM",
    ]):
        text(560, 190 + i * 20, "\u2022 " + line, 10, INK, mono=True)
    text(661, 262, "parses OpenAI-shaped usage;", 9, DENY, "600", "middle")
    text(661, 276, "Bedrock-native meters ZERO today", 9, DENY, "600", "middle")

    # lane B: CloudWatch, out-of-path accounting
    frame(545, 316, 232, 112, "#c9d2db", "#f8fafb", "4 3", 1.4)
    text(661, 342, "CloudWatch logs", 13, INK, "700", "middle")
    text(661, 359, "out of path \u2014 SEES ONLY", 9.5, MUTED, "700", "middle")
    text(560, 382, "\u2022 Bedrock invocation logs", 10, INK, mono=True)
    text(560, 400, "\u2022 real inputTokens / outputTokens", 10, INK, mono=True)

    # Forge joins both
    frame(838, 178, 210, 214, LINE, "#fff", None, 1.8)
    text(943, 206, "BNK Forge", 14, INK, "700", "middle")
    text(943, 224, "the join point", 10, MUTED, anchor="middle")
    for i, line in enumerate([
        "BNK: who was allowed,",
        "  throttled, refused",
        "AWS: what it cost",
        "  in real tokens",
        "one view, whole estate",
    ]):
        text(852, 250 + i * 19, line, 10, INK, mono=True)
    arrow(779, 208, 836, 245, None, F5)
    arrow(779, 372, 836, 330, None, "#9aa5b1")

    box(838, 424, 210, 50, "Chargeback", "per team \u00b7 per model", LINE)
    arrow(943, 392, 943, 424)

    # the point
    frame(40, 500, 1140, 84, "#e3e8ee", PANEL, None, 1)
    text(60, 528, "\u201cWhat is our AI spend, by team, and who is about to blow the budget?\u201d",
         13, INK, "700")
    text(60, 552, "The Gateway limiter answers it for one row. BNK + Forge answers it for every row \u2014 "
                  "enforcing on the ones BNK fronts,", 11, MUTED)
    text(60, 570, "accounting for the rest from AWS's own logs.", 11, MUTED)

    text(40, 618, "HONEST LIMITS", 10.5, DENY, "700")
    for i, line in enumerate([
        "Enforcement needs BNK in the path. Rows it does not front are visible but not stoppable.",
        "BNK token counting parses OpenAI-shaped usage; Bedrock-native responses meter as zero today (F5 feature request).",
        "The CloudWatch lane is accounting after the fact \u2014 it reports spend, it cannot prevent it.",
        "AgentCore Gateway limits are real and cover requests, tokens AND connections \u2014 but only on its own path,",
        "   scoped to /v1/chat|messages|responses, excluding pass-through targets, and they fail open by design.",
    ]):
        text(40, 638 + i * 18, ("\u2022 " if not line.startswith("  ") else "") + line, 10.5, MUTED)

    out.append("</svg>")
    return "\n".join(out)


def build_network():
    """VPC / subnet / component layout, with the three paths drawn on it.

    Every address and every security-group fact here was read off the live
    cluster and, where behaviour is claimed, tested. If the cluster is rebuilt,
    re-check: subnet CIDRs are assigned by awsbnkctl and stable, but ENI and
    pod addresses are not.

    Layout rules that keep it readable — please preserve them if you edit:
      - band geometry lives in the A_Y..E_Y constants below. Change spacing
        there, not by nudging individual elements.
      - subnet labels sit ABOVE their frame (or right-aligned on the tinted
        BNK bands), so no arrow ever crosses a label.
      - the ~70px gap between bands is a deliberate arrow routing lane.
      - AWS-owned ENIs are drawn in the AWS colour INSIDE our subnet frames
        and linked to the AWS-operated service that owns them.
      - the three paths are drawn heavier than BNK's internal plumbing, so the
        thing the diagram is about reads first.
    """
    out.clear()
    W4, H4 = 1620, 1386
    TINT = "#fff5f6"

    # band geometry — the single place to change spacing
    A_Y, A_H = 176, 130          # private subnets
    B_Y, B_H = 376, 120          # bnk-ext
    C_Y, C_H = 566, 90           # bnk-int
    D_Y, D_H = 726, 194          # public subnets
    F_Y, F_H = 980, 92           # the out-of-range stranger subnet
    E_Y, E_H = 1116, 88          # in-VPC services
    VPC_Y, VPC_H = 104, 1122
    LEFT, RIGHT = 306, 1505      # inner content edges

    out.append(
        f'<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 {W4} {H4}" width="{W4}" height="{H4}">'
    )
    marks = "".join(
        f'<marker id="m{k}" viewBox="0 0 10 10" refX="9" refY="5" markerWidth="7" '
        f'markerHeight="7" orient="auto-start-reverse">'
        f'<path d="M 0 0 L 10 5 L 0 10 z" fill="{v}"/></marker>'
        for k, v in (("l", LINE), ("f", F5), ("o", OK), ("i", INK), ("a", AWS), ("d", DENY))
    )
    out.append(f"<defs>{marks}</defs>")
    out.append(f'<rect width="{W4}" height="{H4}" fill="{BG}"/>')

    mk = {LINE: "ml", F5: "mf", OK: "mo", INK: "mi", AWS: "ma", DENY: "md"}

    def ar(pts, colour=LINE, dash=None, label=None, lx=None, ly=None,
           lanchor="middle", w=2.0):
        d = f' stroke-dasharray="{dash}"' if dash else ""
        pl = " ".join(f"{x},{y}" for x, y in pts)
        out.append(
            f'<polyline points="{pl}" fill="none" stroke="{colour}" stroke-width="{w}" '
            f'stroke-linejoin="round" marker-end="url(#{mk[colour]})"{d}/>'
        )
        if label:
            mx = lx if lx is not None else (pts[0][0] + pts[-1][0]) / 2
            my = ly if ly is not None else (pts[0][1] + pts[-1][1]) / 2
            text(mx, my, label, 11.5, colour, "700", lanchor, mono=True)

    def sublabel(x, y, name, cidr, az, anchor="start"):
        text(x, y, f"{name}  ·  {cidr}  ·  {az}", 11.5, INK, "700", anchor)

    text(40, 46, "Where BNK sits in the VPC", 24, INK, "700")
    text(40, 72, "bnk-agentcore-demo · vpc-06dbebcc9fe3e1bad · 10.0.0.0/16 · ap-southeast-2 · "
                 "addresses and SG behaviour verified live", 12.5, MUTED)

    # ── AWS-operated, genuinely outside the VPC ──────────────────────────────
    text(40, 116, "AWS-OPERATED · NO PACKET PATH FOR US", 10, MUTED, "700")
    box(40, 128, 212, 54, "Amazon Bedrock", "Converse · tool_use", AWS)
    box(40, 202, 212, 60, "AgentCore Runtime", "microVM", AWS)
    box(40, 342, 212, 54, "AgentCore Gateway", "not deployed", MUTED, dash="5 4")
    box(40, 416, 212, 54, "VPC Lattice res-gw", "would add ENIs too", MUTED, dash="5 4")
    ar([(146, 396), (146, 414)], LINE, dash="4 3")
    text(40, 500, "Path 2 stops here: AgentCore trusts", 10, DENY, "600")
    text(40, 514, "public CAs only, and our cert is", 10, DENY, "600")
    text(40, 528, "issued by an in-cluster CA.", 10, DENY, "600")

    # ── the VPC ──────────────────────────────────────────────────────────────
    frame(290, VPC_Y, 1230, VPC_H, LINE, "#fcfdfe", None, 1.8)
    text(LEFT, VPC_Y + 28, "VPC  10.0.0.0/16   —   everything in this box is ours to control",
         13.5, INK, "700")
    text(RIGHT, VPC_Y + 28, "IGW igw-00ca152dbf662b6a9", 10, MUTED, anchor="end", mono=True)

    # ── Band A: private subnets — where AWS parks its ENIs ───────────────────
    sublabel(LEFT + 4, A_Y - 10, "subnet-private-1", "10.0.11.0/24", "az 2a")
    frame(LEFT, A_Y, 590, A_H, LINE, PANEL)
    box(320, A_Y + 16, 270, 48, "AgentCore runtime ENI", "10.0.11.15 · agentic_ai", AWS)
    box(610, A_Y + 16, 272, 48, "EKS control plane ENI", "10.0.11.20", LINE)
    box(320, A_Y + 74, 200, 44, "Lambda ENI", "10.0.11.228", LINE)
    text(700, A_Y + 92, "AWS owns these ENIs,", 9.5, AWS, "600")
    text(700, A_Y + 106, "but they sit in OUR subnet.", 9.5, AWS, "600")
    text(700, A_Y + 120, "That seam is BNK's to police.", 9.5, AWS, "600")

    sublabel(934, A_Y - 10, "subnet-private-2", "10.0.12.0/24", "az 2b")
    frame(930, A_Y, 575, A_H, LINE, PANEL)
    box(944, A_Y + 16, 270, 48, "AgentCore runtime ENI", "10.0.12.191 · agentic_ai", AWS)
    box(1234, A_Y + 16, 258, 48, "EKS control plane ENI", "10.0.12.193", LINE)
    box(944, A_Y + 74, 200, 44, "Lambda ENI", "10.0.12.248", LINE)
    text(1300, A_Y + 92, "the SAME agent as", 9.5, DENY, "600")
    text(1300, A_Y + 106, "private-1 — one agent,", 9.5, DENY, "600")
    text(1300, A_Y + 120, "two source IPs.", 9.5, DENY, "600")

    ar([(254, A_Y + 64), (300, A_Y + 64), (300, A_Y + 40), (318, A_Y + 40)], AWS,
       label="projects ENIs", lx=258, ly=A_Y + 114, lanchor="start")

    # ── Band B: bnk-ext — the VIP ────────────────────────────────────────────
    frame(LEFT, B_Y, 1199, B_H, F5, TINT, None, 1.8)
    sublabel(RIGHT - 8, B_Y - 10, "subnet-bnk-ext", "10.0.10.0/24", "az 2a", anchor="end")
    text(LEFT + 4, B_Y - 10, "BNK's external side", 11, F5, "700")
    box(330, B_Y + 16, 260, 76, "VIP  10.0.10.150", ":80  ·  :443 TLS", F5, "#fff", lw=2.6)
    box(650, B_Y + 16, 260, 76, "TMM  ens8", "10.0.10.209", F5)
    box(970, B_Y + 16, 280, 76, "jumphost  ens6", "10.0.10.29 · bnk-data SG", LINE, dash="4 3")
    text(620, B_Y + 110, "the VIP is a secondary IP on the TMM ENI — plain VPC routing reaches it",
         9.5, MUTED, mono=True)
    text(1270, B_Y + 24, "the OLD stranger. Inside", 9.5, MUTED)
    text(1270, B_Y + 38, "bnk-data, so it proved", 9.5, MUTED)
    text(1270, B_Y + 52, "little. Kept for ops", 9.5, MUTED)
    text(1270, B_Y + 66, "access only.", 9.5, MUTED)

    # ── Band C: bnk-int — TMM's second NIC ───────────────────────────────────
    frame(LEFT, C_Y, 1199, C_H, F5, TINT, None, 1.8)
    sublabel(RIGHT - 8, C_Y - 10, "subnet-bnk-int", "10.0.20.0/24", "az 2a", anchor="end")
    text(LEFT + 4, C_Y - 10, "BNK's internal side", 11, F5, "700")
    box(650, C_Y + 20, 260, 56, "TMM  ens7", "10.0.20.171", F5)
    text(940, C_Y + 40, "The same f5-tmm pod, second NIC, DPDK over vfio.", 10.5, MUTED)
    text(940, C_Y + 58, "That is what the host-device pattern means in addresses.", 10.5, MUTED)

    # ── Band D: public subnets — nodes and pods ──────────────────────────────
    sublabel(LEFT + 4, D_Y - 10, "subnet-public-1", "10.0.1.0/24", "az 2a")
    frame(LEFT, D_Y, 800, D_H, LINE, PANEL)
    box(320, D_Y + 34, 250, 64, "node 10.0.1.11", "f5-tmm", F5)
    box(594, D_Y + 34, 250, 64, "node 10.0.1.32", "no demo workload", LINE)
    box(868, D_Y + 34, 224, 64, "node 10.0.1.181", "runs the MCP pod", LINE)
    box(868, D_Y + 120, 224, 50, "mcp pod 10.0.1.62", None, OK)
    text(320, D_Y + 126, "Pod IPs come out of THIS subnet,", 10, MUTED)
    text(320, D_Y + 142, "not a separate pod network —", 10, MUTED)
    text(320, D_Y + 158, "VPC CNI prefix delegation.", 10, MUTED)
    text(320, D_Y + 184, "NAT 10.0.1.176 · EICE 10.0.1.33 · jumphost ens5 10.0.1.187",
         9.5, MUTED, mono=True)

    sublabel(1144, D_Y - 10, "subnet-public-2", "10.0.2.0/24", "az 2b")
    frame(1140, D_Y, 365, D_H, LINE, PANEL)
    box(1154, D_Y + 20, 337, 58, "the stranger", "t3.micro · 10.0.2.6", INK)
    text(1154, D_Y + 96, "Its own SG. Nothing else is in it.", 10, MUTED)
    text(1154, D_Y + 111, "bnk-data admits it on tcp/443 only —", 10, MUTED)
    text(1154, D_Y + 126, "no shared SG, no subnet adjacency.", 10, MUTED)
    text(1154, D_Y + 152, "Its second NIC is in the subnet", 10, DENY, "600")
    text(1154, D_Y + 167, "below — same host, other range.", 10, DENY, "600")

    sublabel(LEFT + 4, F_Y - 10, "subnet-stranger-outside", "100.64.2.0/24", "az 2b")
    frame(LEFT, F_Y, 1199, F_H, DENY, "#fffafa", "5 4", 1.6)
    box(1154, F_Y + 18, 337, 52, "stranger's 2nd NIC", "100.64.2.43", DENY, "#fff", dash="5 4")
    text(320, F_Y + 26, "A secondary VPC CIDR, deliberately OUTSIDE the firewall's accept list.",
         11, INK, "700")
    text(320, F_Y + 46, "Routable to the VIP (AWS adds a local route), so the packets arrive — "
                        "and the firewall refuses them.", 10.5, MUTED)
    text(320, F_Y + 66, "Same host, same SG, same port as 10.0.2.6. Only the source range "
                        "differs, which is what makes it a test and not an assertion.",
         10.5, MUTED)

    # ── Band E: in-VPC services ──────────────────────────────────────────────
    frame(LEFT, E_Y, 1199, E_H, LINE, "#fff")
    text(320, E_Y + 26, "Route 53 private zone", 11, INK, "700")
    text(320, E_Y + 44, "bnk-demo.internal → 10.0.10.150", 10, MUTED, mono=True)
    text(320, E_Y + 66, "resolves inside the VPC only", 9.5, MUTED)
    text(660, E_Y + 26, "NAT gateway", 11, INK, "700")
    text(660, E_Y + 44, "nat-041ed9c3206186bac", 10, MUTED, mono=True)
    text(660, E_Y + 66, "egress only, no inbound path", 9.5, MUTED)
    text(1000, E_Y + 26, "Security group  bnk-data", 11, INK, "700")
    text(1000, E_Y + 44, ":80/:443 from the agent SG  +  ALL from itself", 10, MUTED, mono=True)
    text(1000, E_Y + 66, "this, not routing, is what admits path 3", 9.5, DENY, "600")

    # ── the paths (heavier) and BNK's plumbing (lighter) ─────────────────────
    PW = 2.8
    ar([(560, A_Y + 66), (560, B_Y + 14)], OK, label="path 1",
       lx=568, ly=B_Y - 46, lanchor="start", w=PW)
    ar([(1180, A_Y + 66), (1180, A_Y + 160), (500, A_Y + 160), (500, B_Y + 14)],
       OK, dash="7 5", w=PW)
    # Path 3 and the reject both run out to lanes beyond the VPC frame, then
    # back in underneath band B, so they cross no label and no box. They land on
    # the VIP side by side: same destination, opposite outcome.
    ar([(1493, D_Y + 49), (1540, D_Y + 49), (1540, 528), (460, 528), (460, 470)],
       INK, label="path 3  →  200", lx=1250, ly=522, w=PW)
    ar([(1493, F_Y + 44), (1580, F_Y + 44), (1580, 506), (540, 506), (540, 470)],
       DENY, dash="5 4", label="out of range  →  RST", lx=1250, ly=500, w=PW)
    ar([(254, 443), (300, 443), (300, B_Y + 54), (326, B_Y + 54)], F5, dash="7 5",
       label="path 2", lx=258, ly=468, lanchor="start", w=PW)

    ar([(592, B_Y + 54), (646, B_Y + 54)], F5)
    ar([(780, B_Y + 94), (780, C_Y + 18)], F5)
    ar([(780, C_Y + 78), (780, C_Y + 110), (980, C_Y + 110), (980, D_Y + 32)], F5,
       label="to pod", lx=900, ly=C_Y + 104)
    ar([(980, D_Y + 100), (980, D_Y + 118)], LINE)
    ar([(1470, D_Y + 78), (1470, F_Y + 16)], LINE, dash="3 3",
       label="same host", lx=1504, ly=F_Y - 8, lanchor="start")

    # ── legend ───────────────────────────────────────────────────────────────
    ly0 = 1266
    text(40, ly0 - 20, "THE THREE PATHS", 10.5, MUTED, "700")
    for i, (colour, dash, name, desc) in enumerate((
        (OK, None, "1  trusted agent",
         "AgentCore runtime ENI → VIP → pod.  Runs today."),
        (F5, "7 5", "2  double-checked",
         "AgentCore Gateway → Lattice ENIs → VIP → pod.  Not built — needs a publicly trusted cert."),
        (INK, None, "3  stranger",
         "stranger in public-2, own SG → VIP → pod.  Runs today."),
    )):
        y = ly0 + i * 26
        d = f' stroke-dasharray="{dash}"' if dash else ""
        out.append(f'<line x1="40" y1="{y}" x2="92" y2="{y}" stroke="{colour}" '
                   f'stroke-width="{PW}" marker-end="url(#{mk[colour]})"{d}/>')
        text(104, y + 4, name, 11.5, INK, "700")
        text(254, y + 4, desc, 11, MUTED)

    text(40, 1362, "Tested live: from 10.0.2.6 the firewall ACCEPTS (200); from 100.64.2.43 "
                   "it REJECTS with a TCP reset (curl rc=7). Same host, same SG, same port — so "
                   "the only", 11, DENY, "600")
    text(40, 1378, "variable is the source range, and a reset (not a timeout) means the "
                   "firewall actively refused rather than the packet being lost. That is the "
                   "fourth refusal, proven rather than asserted.", 11, DENY, "600")
    out.append("</svg>")
    return "\n".join(out)


if __name__ == "__main__":
    ap = argparse.ArgumentParser()
    ap.add_argument("--outdir", default=None)
    args = ap.parse_args()
    outdir = pathlib.Path(args.outdir) if args.outdir else \
        pathlib.Path(__file__).resolve().parent.parent / "images"
    outdir.mkdir(parents=True, exist_ok=True)
    for name, fn in (("three-paths.svg", build),
                     ("estate-token-governance.svg", build_estate),
                     ("network-and-paths.svg", build_network)):
        dest = outdir / name
        dest.write_text(fn(), encoding="utf-8")
        print(f"wrote {dest} ({dest.stat().st_size:,} bytes)")
