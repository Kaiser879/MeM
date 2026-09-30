#!/usr/bin/env bash
# mem demo — the three-beat story, end to end.
#
#   1. Indexing    the tool understands a messy folder
#   2. Retrieval   find a file whose name tells you nothing
#   3. Rediscovery  you never needed to know where it was stored
#
# Usage:  ./demo.sh [directory]     (defaults to ~/Downloads)

set -u
DIR="${1:-$HOME/Downloads}"
MEM="$(cd "$(dirname "$0")" && pwd)/mem"

if [ ! -x "$MEM" ]; then
  echo "Build first:  go build -o mem ."
  exit 1
fi

rule() { printf '\n\033[1m%s\033[0m\n' "$(printf '=%.0s' {1..66})"; }

rule
echo " mem  —  find files by what you remember"
echo " directory: $DIR"
rule

echo
echo "### 1. INDEXING"
echo 'The messiest kind of folder: unnamed PDFs, screenshots, duplicates.'
"$MEM" scan "$DIR"

echo
echo "### 2. RETRIEVAL"
echo 'Query with an unhelpful description and no known filename:'
echo
"$MEM" search "the PDF about Linux process scheduling" --limit 3

echo
echo "### 3. REDISCOVERY"
echo 'A syllabus remembered only by its subject matter:'
echo
"$MEM" search "first year engineering syllabus subjects" --limit 3

echo
echo "### Related memories"
echo 'Connected to something you already found:'
echo
"$MEM" related "1st Year Syllabus.pdf"

echo
echo "### The index so far"
"$MEM" stats

rule
echo " Next: try your own memory —  mem search \"...\""
rule
echo
