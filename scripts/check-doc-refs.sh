#!/usr/bin/env bash
# check-doc-refs.sh — verify every Go-file:line reference in docs/ still resolves.
#
# Extracts patterns and checks that each referenced file exists and the line
# numbers are within range. Bare filenames are resolved via find with
# multi-candidate fallback.
#
# Exits 0 when every reference is valid; prints failures and exits 1 otherwise.

set -uo pipefail
cd "$(dirname "$0")/.."

FAILURES=0
TMPDIR="$(mktemp -d)"
trap 'rm -rf "$TMPDIR"' EXIT

fail() {
  echo "REF-FAIL: $*" >&2
  FAILURES=$((FAILURES + 1))
}

# resolve a Go file path.
resolve_go_file() {
  local raw="$1"
  local path
  path="$(echo "$raw" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//' -e 's/^`//' -e 's/`$//')"

  # Direct match
  if [[ -f "$path" ]]; then
    echo "$path"
    return
  fi

  # Path with directory — try under common prefixes
  if [[ "$path" == */* ]]; then
    local candidate
    for prefix in "" "internal/" "cmd/"; do
      candidate="${prefix}${path}"
      if [[ -f "$candidate" ]]; then
        echo "$candidate"
        return
      fi
    done
  fi

  # Bare filename — search
  local matches
  matches="$(find internal cmd -name "$path" -type f 2>/dev/null || true)"
  if [[ -n "$matches" ]]; then
    echo "$matches"
    return
  fi

  echo ""
}

# Try each file candidate until one passes the line number check.
check_ref_multi() {
  local files_str="$1"
  local start_line="$2"
  local end_line="${3:-$start_line}"
  local ref_label="$4"

  local found=0
  while IFS= read -r file; do
    [[ -z "$file" ]] && continue
    if [[ ! -f "$file" ]]; then
      continue
    fi
    local total
    total="$(wc -l < "$file")"
    if [[ "$start_line" -ge 1 ]] && [[ "$start_line" -le "$total" ]]; then
      if [[ -z "$end_line" ]] || [[ "$end_line" -le "$total" ]]; then
        found=1
        break
      fi
    fi
  done <<< "$files_str"

  if [[ "$found" -eq 0 ]]; then
    local first
    first="$(echo "$files_str" | head -1)"
    if [[ ! -f "$first" ]]; then
      fail "$ref_label: file not found"
    else
      local total
      total="$(wc -l < "$first")"
      fail "$ref_label: $first:$start_line-$end_line exceeds file length ($total lines)"
    fi
    return 1
  fi
  return 0
}

# Join continuation lines in a markdown file: a line ending with "第 NNN" or
# "第 NNN–MMM" without a trailing "行" is continued on the next line.
join_continuations() {
  local infile="$1"
  local outfile="$2"
  awk '
    BEGIN { buf="" }
    {
      if (buf != "") {
        # Previous line was a partial reference — join
        buf = buf " " $0
        if ($0 ~ /行/) { print buf; buf = "" }
        next
      }
      # Line ends with 第 NNN or 第 NNN–MMM without trailing 行
      if ($0 ~ /第[[:space:]]*[0-9]+[–\-]?[0-9]*[[:space:]]*$/) {
        buf = $0
        next
      }
      print $0
    }
    END { if (buf != "") print buf }
  ' "$infile" > "$outfile"
}

# Process one .md file.
process_file() {
  local md="$1"
  local joined="$TMPDIR/$(basename "$md").joined"
  join_continuations "$md" "$joined"

  local prev_go_file=""

  while IFS= read -r line; do
    # Only process lines with 第...行
    if ! echo "$line" | grep -qP '第[[:space:]]*\d+.*行'; then
      continue
    fi

    # Extract go file from this line (may be absent for 同文件)
    local go_file
    go_file="$(echo "$line" | grep -oP '`[^`]*\.go`' | head -1 | sed -e 's/^`//' -e 's/`$//' || true)"

    if [[ -n "$go_file" ]]; then
      local resolved
      resolved="$(resolve_go_file "$go_file")"
      if [[ -z "$resolved" ]]; then
        fail "$md: cannot resolve file '$go_file'"
        continue
      fi
      # Keep every candidate: a bare filename like `store.go` matches several
      # packages, and a later "第 NNN 行" reference with no file must validate
      # against whichever candidate actually fits the line numbers.
      prev_go_file="$resolved"
    fi

    # Check for 同文件 (same file as previous)
    local same_file=0
    if echo "$line" | grep -qP '同文件[[:space:]]*第[[:space:]]*\d+'; then
      same_file=1
      if [[ -z "$prev_go_file" ]]; then
        fail "$md: 同文件 reference but no preceding Go file"
        continue
      fi
    fi

    # Extract all line ranges from this line
    local ranges
    ranges="$(echo "$line" | grep -oP '第[[:space:]]*\d+([–\-]\d+)?[[:space:]]*行' || true)"
    if [[ -z "$ranges" ]]; then
      continue
    fi

    while IFS= read -r range_text; do
      [[ -z "$range_text" ]] && continue
      local numbers start end
      numbers="$(echo "$range_text" | grep -oP '\d+' | tr '\n' ' ' || true)"
      start="$(echo "$numbers" | awk '{print $1}')"
      end="$(echo "$numbers" | awk '{print $2}')"
      [[ -z "$start" ]] && continue

      if [[ "$same_file" -eq 1 ]] || [[ -z "$go_file" ]]; then
        check_ref_multi "$prev_go_file" "$start" "${end:-$start}" "$md:同文件"
      else
        check_ref_multi "$resolved" "$start" "${end:-$start}" "$md:$go_file"
      fi
    done <<< "$ranges"
  done < "$joined"
}

# main
DOCS_DIR="docs"
if [[ ! -d "$DOCS_DIR" ]]; then
  echo "docs/ directory not found" >&2
  exit 1
fi

while IFS= read -r -d '' md; do
  process_file "$md"
done < <(find "$DOCS_DIR" -name '*.md' -print0)

if [[ "$FAILURES" -gt 0 ]]; then
  echo "check-doc-refs: $FAILURES reference(s) failed" >&2
  exit 1
fi

echo "check-doc-refs: all references ok"
