#!/usr/bin/env bash
# Gives ADRs their numbers at the merge, so that sessions working at once
# never choose one (two were numbered 0053 in one evening). A branch writes
# a new record as docs/adr/new-SLUG.md and calls it "ADR new-SLUG"; whoever
# merges, on the merged tree:
#
#   scripts/adr-number.sh                  each docs/adr/new-SLUG.md gets the next
#                                          free number, and every reference to it
#   scripts/adr-number.sh OLD NEW [BASE]   a numbered ADR this branch added since
#                                          BASE (its merge base with origin/main)
#                                          becomes NEW; references are rewritten
#                                          only in lines the branch added
#
# It changes the working tree and stages the renames; it commits nothing.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
die() { echo "adr-number: $*" >&2; exit 1; }

next() { ls docs/adr | sed -n 's/^\([0-9]\{4\}\)-.*/\1/p' | sort -n | tail -1 | awk '{printf "%04d", $1 + 1}'; }

if [ $# -eq 0 ]; then
    shopt -s nullglob
    news=(docs/adr/new-*.md)
    [ ${#news[@]} -gt 0 ] || { echo "adr-number: no docs/adr/new-*.md to number"; exit 0; }
    for file in "${news[@]}"; do
        slug="$(basename "$file" .md)"; slug="${slug#new-}"
        n="$(next)"
        git mv "$file" "docs/adr/$n-$slug.md"
        # The file's name first, then the name standing alone: ADR new-SLUG, [new-SLUG].
        git grep -l -F "new-$slug" | while IFS= read -r f; do
            python3 - "$f" "$slug" "$n" <<'PY'
import re, sys
path, slug, n = sys.argv[1:]
s = open(path, encoding="utf-8").read()
s = s.replace(f"new-{slug}.md", f"{n}-{slug}.md")
s = re.sub(rf"(?<![\w/-])new-{re.escape(slug)}(?![\w-])", n, s)
open(path, "w", encoding="utf-8").write(s)
PY
        done
        echo "adr-number: new-$slug is ADR $n"
    done
    exit 0
fi

[ $# -ge 2 ] || die "usage: scripts/adr-number.sh [OLD NEW [BASE]]"
old="$1" new="$2"
base="${3:-$(git merge-base HEAD origin/main)}"
[[ "$old" =~ ^[0-9]{4}$ && "$new" =~ ^[0-9]{4}$ ]] || die "a number is four digits"
shopt -s nullglob
from=(docs/adr/"$old"-*.md)
[ ${#from[@]} -gt 0 ] || die "no ADR $old"
[ -z "$(ls docs/adr | grep "^$new-" || true)" ] || die "ADR $new is taken"
# This branch's own: one the merge base does not have.
mine=""
for f in "${from[@]}"; do git cat-file -e "$base:$f" 2>/dev/null || mine="$f"; done
[ -n "$mine" ] || die "ADR $old is not one this branch added since $base"
git mv "$mine" "docs/adr/$new-${mine#docs/adr/$old-}"
# Only lines added since the merge base: there, $old can only be this branch's.
git diff -U0 --no-renames "$base" --name-only --diff-filter=AM | while IFS= read -r f; do
    lines="$(git diff -U0 --no-renames "$base" -- "$f" | sed -n 's/^@@ -[0-9,]* +\([0-9]*\)\(,\([0-9]*\)\)\{0,1\} @@.*/\1 \3/p')"
    python3 - "$f" "$old" "$new" "$mine" <<PY
import re, sys
path, old, new, mine = sys.argv[1:]
ranges = []
for l in """$lines""".split("\n"):
    if l.strip():
        start, _, count = l.partition(" ")
        count = int(count) if count else 1
        ranges.append((int(start), int(start) + count))
lines = open(path, encoding="utf-8").read().split("\n")
for i in range(len(lines)):
    if any(a <= i + 1 < b for a, b in ranges):
        lines[i] = re.sub(rf"(?<![\w.-]){old}(?![\w])", new, lines[i])
open(path, "w", encoding="utf-8").write("\n".join(lines))
PY
done
echo "adr-number: ADR $old of this branch is ADR $new"
