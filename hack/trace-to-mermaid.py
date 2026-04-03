#!/usr/bin/env python3
"""Fetch the latest Jaeger trace for a service and render it as a Mermaid Gantt chart.

Usage: trace-to-mermaid.py <service_name> [min_ms]

Arguments:
  service_name  Jaeger service name (e.g., "nerdbox")
  min_ms        Minimum span duration in ms to include (default: 1)

Environment:
  JAEGER_URL    Jaeger base URL (default: http://localhost:16686)
"""
import json
import sys
import urllib.parse
import urllib.request

if len(sys.argv) < 2:
    print("Usage: trace-to-mermaid.py <service_name> [min_ms]", file=sys.stderr)
    sys.exit(1)

service = sys.argv[1]
min_ms = int(sys.argv[2]) if len(sys.argv) > 2 else 1
jaeger_url = __import__("os").environ.get("JAEGER_URL", "http://localhost:16686")

# Fetch latest trace for the service
url = f"{jaeger_url}/api/traces?service={urllib.parse.quote_plus(service)}&limit=1"
try:
    with urllib.request.urlopen(url, timeout=10) as resp:
        trace_data = json.load(resp)
except Exception as e:
    print(f"Error: could not fetch traces from {jaeger_url}: {e}", file=sys.stderr)
    print("Is Jaeger running? Try: make jaeger-start", file=sys.stderr)
    sys.exit(1)

if not trace_data.get("data"):
    print(f"Error: no traces found for service '{service}'", file=sys.stderr)
    print("Run some operations first, e.g.:", file=sys.stderr)
    print("  make test-tracing", file=sys.stderr)
    sys.exit(1)

trace = trace_data["data"][0]
spans = trace["spans"]
processes = trace["processes"]

if not spans:
    print("Error: trace has no spans", file=sys.stderr)
    sys.exit(1)

# Find trace start time (earliest span)
trace_start = min(s["startTime"] for s in spans)

# Build span lookup and parent map
span_map = {}
children = {}
for s in spans:
    sid = s["spanID"]
    span_map[sid] = s
    children.setdefault(sid, [])
    for ref in s.get("references", []):
        if ref["refType"] == "CHILD_OF":
            parent_id = ref["spanID"]
            children.setdefault(parent_id, [])
            children[parent_id].append(sid)

# Find root spans (no CHILD_OF reference within this trace)
all_span_ids = {s["spanID"] for s in spans}
root_spans = []
for s in spans:
    refs = s.get("references", [])
    parent_refs = [r for r in refs if r["refType"] == "CHILD_OF" and r["spanID"] in all_span_ids]
    if not parent_refs:
        root_spans.append(s)

# Sort roots by start time
root_spans.sort(key=lambda s: s["startTime"])


def span_duration_ms(s):
    return s["duration"] / 1000.0


def span_start_ms(s):
    return (s["startTime"] - trace_start) / 1000.0


def safe_id(name, sid):
    """Create a mermaid-safe task ID."""
    clean = name.replace(" ", "_").replace(".", "_").replace("/", "_").replace("-", "_")
    # Append short span ID suffix for uniqueness
    return f"{clean}_{sid[:6]}"


def collect_spans(span_id, depth=0):
    """Collect span and descendants, depth-first."""
    s = span_map[span_id]
    result = [(s, depth)]
    kids = children.get(span_id, [])
    kids.sort(key=lambda cid: span_map[cid]["startTime"])
    for cid in kids:
        result.extend(collect_spans(cid, depth + 1))
    return result


# Build output
service_name = processes[spans[0]["processID"]]["serviceName"]
lines = []
lines.append("```mermaid")
lines.append("gantt")
lines.append("    dateFormat x")
lines.append("    axisFormat %s.%L s")
lines.append(f"    title Trace for {service_name}")
lines.append("")

for root in root_spans:
    root_name = root["operationName"]

    # Section per root span
    lines.append(f"    section {root_name}")

    all_spans = collect_spans(root["spanID"])
    for s, depth in all_spans:
        dur_ms = span_duration_ms(s)
        if dur_ms < min_ms:
            continue
        start_ms = span_start_ms(s)
        end_ms = start_ms + dur_ms

        # Indent name by depth for readability
        prefix = ". " * depth
        op = s["operationName"]
        proc = processes[s["processID"]]["serviceName"]
        label = f"{prefix}{op}"
        if proc != service_name:
            label = f"{prefix}{proc}/{op}"

        tid = safe_id(op, s["spanID"])
        # Mermaid gantt uses milliseconds with dateFormat x
        lines.append(f"    {label} :{tid}, {int(start_ms)}, {int(end_ms)}")

lines.append("```")

print("\n".join(lines))
