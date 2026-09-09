#!/usr/bin/env bash
# check-tool-count.sh — verify the MCP tool count agrees between the actual
# registrations in cmd/nexusgate-mcp/main.go and every manifest/doc that
# states it in prose. A plugin manifest advertising a stale tool count after
# a tool was added or removed is exactly the kind of drift an audit had to
# catch by hand once; this makes the next one fail CI instead.
set -euo pipefail

root=$(CDPATH= cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
main_go="$root/cmd/nexusgate-mcp/main.go"

if [[ ! -f "$main_go" ]]; then
  printf '%s not found\n' "$main_go" >&2
  exit 1
fi

registered=$(grep -c 'AddTool(' "$main_go" || true)
if [[ -z "$registered" || "$registered" -eq 0 ]]; then
  printf 'found zero AddTool( registrations in %s; the tool-count check cannot run\n' "$main_go" >&2
  exit 1
fi

number_word() {
  case "$1" in
    1) echo one ;;
    2) echo two ;;
    3) echo three ;;
    4) echo four ;;
    5) echo five ;;
    6) echo six ;;
    7) echo seven ;;
    8) echo eight ;;
    9) echo nine ;;
    10) echo ten ;;
    *) echo "" ;;
  esac
}
word=$(number_word "$registered")

# README_CN.md states the same claim in Chinese, so the count has a second
# spelling to keep honest. Without this the Chinese README could drift while
# the English one stayed green -- exactly the half-green failure the two
# locale catalogues already taught us to guard against.
cn_number_word() {
  case "$1" in
    1) echo 一 ;; 2) echo 二 ;; 3) echo 三 ;; 4) echo 四 ;; 5) echo 五 ;;
    6) echo 六 ;; 7) echo 七 ;; 8) echo 八 ;; 9) echo 九 ;; 10) echo 十 ;;
    *) echo "" ;;
  esac
}
cn_word=$(cn_number_word "$registered")

# Locate the count claim in one file: the number word or digit that appears
# up to four words before "tool"/"tools" on the same line (matching how each
# of the four files currently phrases it: "six read-only MCP tools", "current
# six MCP tools", "exactly six tools"). If a future rewrite moves the number
# after "tools", onto a separate line, or drops it in favor of some other
# phrasing, this stops matching — that is meant to be a failure (a hidden
# claim is not a passing claim), not something for this script to work around
# silently.
find_claim() {
  local file="$1"
  grep -ioE '(one|two|three|four|five|six|seven|eight|nine|ten|[0-9]+)([[:space:]]+[[:alpha:]][[:alpha:]-]*){0,4}[[:space:]]+tools?\b' "$file" \
    | head -1 \
    | grep -ioE '^(one|two|three|four|five|six|seven|eight|nine|ten|[0-9]+)' && return 0
  # Chinese form: <numeral>个 ... 工具 (the measure word 个 sits between them).
  grep -oE '(一|二|三|四|五|六|七|八|九|十|[0-9]+)个[^ ]{0,12}工具' "$file" \
    | head -1 \
    | grep -oE '^(一|二|三|四|五|六|七|八|九|十|[0-9]+)'
}

failures=0
for doc in \
  ".claude-plugin/marketplace.json" \
  "plugins/claude/.claude-plugin/plugin.json" \
  "plugins/claude/README.md" \
  "README.md" \
  "README_CN.md"
do
  path="$root/$doc"
  if [[ ! -f "$path" ]]; then
    printf '%s not found\n' "$path" >&2
    failures=$((failures + 1))
    continue
  fi
  claim=$(find_claim "$path" || true)
  if [[ -z "$claim" ]]; then
    printf '%s: could not locate a tool-count claim near the word "tool(s)"; a rephrase that hides the claim is a failure, not a pass\n' "$doc" >&2
    failures=$((failures + 1))
    continue
  fi
  claim_lc=$(printf '%s' "$claim" | tr '[:upper:]' '[:lower:]')
  if [[ "$claim_lc" == "$registered" || "$claim_lc" == "$word" || "$claim_lc" == "$cn_word" ]]; then
    continue
  fi
  printf '%s claims %s tools, but %s registers %s (%s) via AddTool(\n' "$doc" "$claim_lc" "cmd/nexusgate-mcp/main.go" "$registered" "$word" >&2
  failures=$((failures + 1))
done

if [[ "$failures" -gt 0 ]]; then
  exit 1
fi
printf 'tool count: %s (%s), consistent across manifests and docs\n' "$registered" "$word"
