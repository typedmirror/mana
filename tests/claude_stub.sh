#!/bin/sh
# Deterministic stand-in for the claude CLI (D-047).
#
# A golden must not embed a model's prose for the same reason it must not
# embed the clock. The stub keeps the Real pathway honest — flags, stdin
# delivery, exit codes, JSON decode — while answering from a fixed table.
# Flags are accepted and ignored; the prompt arrives on stdin, exactly as it
# does for the real thing.
prompt=$(cat)
case "$prompt" in
  *refuse*)      echo "stub: refusing as instructed" >&2; exit 1 ;;
  *correctness*) echo '{ "verdict": "pass", "note": "the logic holds" }' ;;
  *security*)    echo '{ "verdict": "pass", "note": "no injection path" }' ;;
  *clarity*)     echo '{ "verdict": "fail", "note": "two acts overlap" }' ;;
  *)             echo "ok" ;;
esac
