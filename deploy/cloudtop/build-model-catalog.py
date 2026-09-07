#!/opt/homebrew/bin/python3
"""Generate the private Zed catalog from the installed Codex Sol metadata."""
import argparse
import copy
import json
import os
from pathlib import Path
import tempfile


OVERRIDES = {
    "slug": "zed/gpt-5.6-sol",
    "display_name": "Sol (Zed)",
    "description": "GPT-5.6 Sol using the Zed account through the local CLIProxy provider.",
    "default_reasoning_level": "medium",
    "max_context_window": 272000,
    "additional_speed_tiers": [],
    "service_tiers": [],
    "include_apps_usage_instructions": False,
    "multi_agent_version": None,
    "supports_search_tool": False,
    "use_responses_lite": False,
}


def build_catalog(cache_path):
    cache = json.loads(cache_path.read_text())
    models = cache.get("models") if isinstance(cache, dict) else None
    if not isinstance(models, list):
        raise ValueError("The Codex cache must contain a models list.")
    matches = [
        model for model in models
        if isinstance(model, dict) and model.get("slug") == "gpt-5.6-sol"
    ]
    if len(matches) != 1:
        raise ValueError(
            "Expected exactly one gpt-5.6-sol entry; refresh the installed Codex model cache."
        )
    model = copy.deepcopy(matches[0])
    model.update(OVERRIDES)
    return {"models": [model]}


def write_catalog(output_path, catalog):
    output_path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    fd, temporary_name = tempfile.mkstemp(
        prefix="." + output_path.name + ".", dir=output_path.parent
    )
    temporary_path = Path(temporary_name)
    try:
        with os.fdopen(fd, "w") as output:
            os.fchmod(output.fileno(), 0o600)
            json.dump(catalog, output, indent=2)
            output.write("\n")
            output.flush()
            os.fsync(output.fileno())
        os.replace(temporary_path, output_path)
    finally:
        temporary_path.unlink(missing_ok=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cache", type=Path, default=Path.home() / ".codex/models_cache.json")
    parser.add_argument("--output", type=Path, default=Path.home() / ".codex/zed-sol.models.json")
    args = parser.parse_args()
    cache_path, output_path = args.cache.expanduser(), args.output.expanduser()
    try:
        if cache_path.resolve() == output_path.resolve():
            raise ValueError("The output must not replace the Codex model cache.")
        write_catalog(output_path, build_catalog(cache_path))
    except (OSError, ValueError) as error:
        parser.exit(1, "Cannot build Zed model catalog: " + str(error) + "\n")
    print("Wrote Zed model catalog: " + str(output_path))


if __name__ == "__main__":
    main()
