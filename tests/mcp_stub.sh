#!/bin/sh
# Deterministic MCP server for the bridge tests (D-053). Speaks
# newline-delimited JSON-RPC on stdio: initialize, tools/list, tools/call.
# Three tools: lookup (canned JSON answer), explode (declared tool failure),
# reflect (echoes the caller's _meta intent back — proof the `--` channel
# crossed the protocol boundary).
while IFS= read -r line; do
  id=$(printf '%s' "$line" | sed -n 's/.*"id":[[:space:]]*\([0-9][0-9]*\).*/\1/p')
  case "$line" in
    *'"initialize"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"protocolVersion":"2025-06-18","capabilities":{"tools":{}},"serverInfo":{"name":"stub","version":"0"}}}\n' "$id" ;;
    *'"notifications/initialized"'*)
      : ;;
    *'"tools/list"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"tools":[{"name":"explode","inputSchema":{"type":"object"}},{"name":"lookup","inputSchema":{"type":"object"}},{"name":"reflect","inputSchema":{"type":"object"}}]}}\n' "$id" ;;
    *'"explode"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"the tool exploded, as designed"}],"isError":true}}\n' "$id" ;;
    *'"reflect"'*)
      intent=$(printf '%s' "$line" | sed -n 's/.*"mana\/intent":[[:space:]]*"\([^"]*\)".*/\1/p')
      printf '{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"intent received: %s"}]}}\n' "$id" "$intent" ;;
    *'"lookup"'*)
      printf '{"jsonrpc":"2.0","id":%s,"result":{"content":[{"type":"text","text":"{ \\"region\\": \\"eu\\", \\"count\\": 3 }"}]}}\n' "$id" ;;
    *)
      if [ -n "$id" ]; then
        printf '{"jsonrpc":"2.0","id":%s,"error":{"code":-32601,"message":"method not found"}}\n' "$id"
      fi ;;
  esac
done
