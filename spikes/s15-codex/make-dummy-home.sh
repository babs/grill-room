#!/usr/bin/env bash
# Builds a CODEX_HOME holding only placeholders: an unsigned fake id_token and access token (exp in
# 10 years, so codex never refreshes proactively) and a config pointing every ChatGPT URL at the
# bearer-swap proxy. Nothing here comes from the real auth.json.
set -euo pipefail

usage() { echo "usage: $0 <dir> <proxy-origin e.g. http://127.0.0.1:18787> <dummy-bearer> [plan]" >&2; exit 2; }
[[ $# -ge 3 ]] || usage
readonly dir="$1" origin="$2" bearer="$3" plan="${4:-team}"

mkdir -p "$dir"
chmod 700 "$dir"
python3 -I - "$dir/auth.json" "$bearer" "$plan" <<'PY'
import base64, json, sys, time
path, bearer, plan = sys.argv[1:4]
def b64(o): return base64.urlsafe_b64encode(json.dumps(o).encode()).rstrip(b"=").decode()
exp = int(time.time()) + 10 * 365 * 86400
auth = {"chatgpt_plan_type": plan, "chatgpt_account_id": "00000000-0000-4000-8000-00000000a001",
        "chatgpt_user_id": "user-grill-room-dummy"}
id_token = ".".join([b64({"alg": "none"}), b64({"email": "agent@grill-room.invalid", "exp": exp,
                     "https://api.openai.com/auth": auth}), "sig"])
# The bearer itself is the access token: the proxy recognises it, upstream never sees it.
json.dump({"auth_mode": "chatgpt", "OPENAI_API_KEY": None,
           "tokens": {"id_token": id_token, "access_token": bearer,
                      "refresh_token": "grill-room-dummy-refresh", "account_id": auth["chatgpt_account_id"]},
           "last_refresh": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())}, open(path, "w"))
PY
chmod 600 "$dir/auth.json"
cat > "$dir/config.toml" <<TOML
model = "gpt-5.6-luna"
model_reasoning_effort = "low"
openai_base_url = "${origin}/backend-api/codex"
chatgpt_base_url = "${origin}/backend-api/"
TOML
