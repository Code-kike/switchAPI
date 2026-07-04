#!/usr/bin/env python3
"""Rebuild the embedded LiteLLM pricing snapshot (研究#4).

Downloads the upstream model_prices_and_context_window.json, keeps only models
whose mode is chat or responses, strips each entry down to the six fields the
pricing engine reads, and writes a compact snapshot with a _meta header.

Usage: build_pricing_snapshot.py <source-url> <out-path>
"""
import json
import sys
import urllib.request
from datetime import date

KEEP_MODES = {"chat", "responses"}
FIELDS = (
    "input_cost_per_token",
    "output_cost_per_token",
    "cache_creation_input_token_cost",
    "cache_read_input_token_cost",
    "litellm_provider",
    "mode",
)


def main() -> int:
    if len(sys.argv) != 3:
        print(__doc__, file=sys.stderr)
        return 2
    url, out_path = sys.argv[1], sys.argv[2]
    with urllib.request.urlopen(url) as resp:
        raw = json.load(resp)

    out = {}
    for model, spec in raw.items():
        if model == "sample_spec" or model.startswith("_"):
            continue
        if not isinstance(spec, dict) or spec.get("mode") not in KEEP_MODES:
            continue
        out[model] = {k: spec[k] for k in FIELDS if k in spec}

    snapshot = {
        "_meta": {
            "fetched_at": date.today().isoformat(),
            "filter": "mode in (chat,responses)",
            "models": len(out),
            "source": "BerriAI/litellm model_prices_and_context_window.json",
        },
        **{k: out[k] for k in sorted(out)},
    }
    with open(out_path, "w") as f:
        json.dump(snapshot, f, separators=(",", ":"), sort_keys=True)
        f.write("\n")
    print(f"wrote {len(out)} models to {out_path}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
