#!/usr/bin/env bash
# Regenerate the bundled first-party plugin-catalog snapshot embedded in tierd.
#
# SmoothNAS ships the manifests for its own plugins with the appliance so the
# "Install plugins" catalog works offline / when GitHub is rate-limited (see
# docs/proposals/pending/plugins-12-bundled-catalog.md). This snapshot is the
# guaranteed-installable floor; a connected appliance still refreshes from each
# repo's latest release for newer versions.
#
# This script fetches each first-party repo's LATEST release, copies the
# manifest assets (the same set tierd's catalog ingests: smoothnas-plugin.yaml
# and smoothnas-plugin-*.yaml — NOT index.json or *-profile.yaml), and writes
# tierd/internal/api/catalogdata/{index.json, <id>/<manifest>.yaml}.
#
# Run from the repo root. Requires `gh` (authenticated) and `python3`.
#   scripts/sync-plugin-catalog.sh
#
# The repo list MUST stay in sync with the frontend catalog list
# (tierd-ui/src/data/pluginCatalog.ts) — the same first-party repos.
set -euo pipefail

# id repo — keep aligned with tierd-ui/src/data/pluginCatalog.ts.
REPOS=(
  "aimee RakuenSoftware/smoothnas-plugin-aimee"
  "gh-runner RakuenSoftware/smoothnas-plugin-gh-runner"
  "llama-cpp RakuenSoftware/smoothnas-plugin-llama-cpp"
  "vllm RakuenSoftware/smoothnas-plugin-vllm"
  "wolf RakuenSoftware/smoothnas-plugin-wolf"
)

root="$(git rev-parse --show-toplevel)"
out="$root/tierd/internal/api/catalogdata"

command -v gh >/dev/null || { echo "gh CLI is required" >&2; exit 1; }
command -v python3 >/dev/null || { echo "python3 is required" >&2; exit 1; }

# A release asset is a plugin manifest iff its basename is smoothnas-plugin.yaml
# / .yml or starts with "smoothnas-plugin-" (mirrors isPluginManifestAsset in
# tierd/internal/api/plugins_catalog.go). This excludes index.json and profile
# assets like wolf-runtime-profile.yaml.
is_manifest() {
  local n="${1,,}"
  case "$n" in
    smoothnas-plugin.yaml|smoothnas-plugin.yml) return 0 ;;
    smoothnas-plugin-*.yaml|smoothnas-plugin-*.yml) return 0 ;;
    *) return 1 ;;
  esac
}

rm -rf "$out"
mkdir -p "$out"

# index.json is assembled incrementally as newline-delimited JSON objects, then
# wrapped into {"repositories":[...]} by python at the end.
tmp_entries="$(mktemp)"
trap 'rm -f "$tmp_entries"' EXIT

for row in "${REPOS[@]}"; do
  id="${row%% *}"
  repo="${row#* }"
  echo "==> $id ($repo)"

  tag="$(gh release view --repo "$repo" --json tagName --jq '.tagName')"
  url="$(gh release view --repo "$repo" --json url --jq '.url')"
  mkdir -p "$out/$id"

  names=()
  while IFS= read -r name; do
    [[ -n "$name" ]] || continue
    is_manifest "$name" || continue
    gh release download --repo "$repo" --pattern "$name" -O "$out/$id/$name" --clobber
    names+=("$name")
    echo "    + $name"
  done < <(gh release view --repo "$repo" --json assets --jq '.assets[].name')

  if [[ ${#names[@]} -eq 0 ]]; then
    echo "    !! no manifest assets in $repo $tag" >&2
    exit 1
  fi

  ID="$id" REPO="$repo" TAG="$tag" URL="$url" MANIFESTS="${names[*]}" python3 - "$tmp_entries" <<'PY'
import json, os, sys
entry = {
    "id": os.environ["ID"],
    "repo": os.environ["REPO"],
    "tagName": os.environ["TAG"],
    "releaseUrl": os.environ["URL"],
    "manifests": os.environ["MANIFESTS"].split(),
}
with open(sys.argv[1], "a") as f:
    f.write(json.dumps(entry) + "\n")
PY
done

python3 - "$tmp_entries" "$out/index.json" <<'PY'
import json, sys
repos = [json.loads(line) for line in open(sys.argv[1]) if line.strip()]
with open(sys.argv[2], "w") as f:
    json.dump({"repositories": repos}, f, indent=2)
    f.write("\n")
PY

echo "==> wrote $out/index.json"
echo "Done. Review the diff, then commit tierd/internal/api/catalogdata/."
