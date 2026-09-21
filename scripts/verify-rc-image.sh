#!/usr/bin/env bash
# Copyright 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

if [[ $# -ne 2 ]]; then
  echo "usage: $0 IMAGE_REF EXPECTED_REVISION" >&2
  exit 64
fi

image_ref="$1"
expected_revision="$2"
temp_root="${RUNNER_TEMP:-${TMPDIR:-/tmp}}"
manifest_file="$(mktemp "$temp_root/dsx-rc-manifest.XXXXXX")"
image_file="$(mktemp "$temp_root/dsx-rc-image.XXXXXX")"
error_file="$(mktemp "$temp_root/dsx-rc-error.XXXXXX")"
trap 'rm -f "$manifest_file" "$image_file" "$error_file"' EXIT

if docker buildx imagetools inspect --format '{{json .Manifest}}' \
  "$image_ref" >"$manifest_file" 2>"$error_file"; then
  :
else
  status=$?
  if grep -Eqi 'manifest unknown|not found|404' "$error_file"; then
    exit 3
  fi
  cat "$error_file" >&2
  exit "$status"
fi

if ! jq -e '
  [.manifests[]?
    | select(.platform.os == "linux")
    | .platform.architecture]
  | unique
  | . as $platforms
  | ($platforms | index("amd64") != null)
    and ($platforms | index("arm64") != null)
' "$manifest_file" >/dev/null; then
  echo "$image_ref does not contain both linux/amd64 and linux/arm64." >&2
  exit 1
fi

docker buildx imagetools inspect --format '{{json .Image}}' \
  "$image_ref" >"$image_file"

if ! jq -e --arg expected "$expected_revision" '
  [..
    | objects
    | select(has("config"))
    | .config
    | select(type == "object")
    | (.Labels // {})["org.opencontainers.image.revision"]
    | select(type == "string")] as $revisions
  | ($revisions | length) >= 2
    and ($revisions | all(. == $expected))
' "$image_file" >/dev/null; then
  echo "$image_ref does not match source revision $expected_revision on every platform." >&2
  exit 1
fi

echo "Verified $image_ref at source revision $expected_revision."
