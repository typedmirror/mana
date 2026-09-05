#!/bin/sh
# Deterministic stand-in for the harmonic CLI (D-064). Speaks the exec
# contract: `exec <kernel> -f <cell>` -> one JSON document on stdout; exit 0
# success, 1 ordinary error, 3 capability denial with the inline ref in
# evalue. The stub is an UNGUARDED kernel — python3 really executes the
# cell — except that any run-cell whose command mentions curl is denied
# before execution, which gives tests a real execution path and a
# deterministic denial path from one artifact.
[ "$1" = "exec" ] || { printf '{"error":"unsupported"}\n'; exit 1; }
shift 2
[ "$1" = "-f" ] || { printf '{"error":"need -f"}\n'; exit 1; }
file=$2
b64=$(sed -n '1s/.*"b64":"\([^"]*\)".*/\1/p' "$file")
decoded=$(printf '%s' "$b64" | base64 -d 2>/dev/null)
case "$decoded" in
  *curl*)
    printf '{"cell_id":1,"status":"denied","outputs":[{"type":"error","ename":"PermissionError","evalue":"harmonic guard: subprocess denied: '\''curl'\'' [harmonic:stub00@reftip0]"}]}\n'
    exit 3 ;;
esac
errf=$(mktemp)
out=$(python3 "$file" 2>"$errf"); rc=$?
err=$(cat "$errf"); rm -f "$errf"
export STUB_RC="$rc" STUB_OUT="$out" STUB_ERR="$err"
python3 - <<'PY'
import json, os
rc = int(os.environ["STUB_RC"])
out = os.environ["STUB_OUT"]
err = os.environ["STUB_ERR"]
o = []
if out:
    o.append({"type": "stream", "name": "Stdout", "text": out + "\n"})
if err:
    o.append({"type": "stream", "name": "Stderr", "text": err})
print(json.dumps({"cell_id": 1, "status": "success" if rc == 0 else "error", "outputs": o}))
PY
[ "$rc" -eq 0 ] && exit 0 || exit 1
