#!/usr/bin/env bash
# Run the GitHub Copilot live suite against the OAuth (device-login) path.
#
#   ./scripts/copilot-live-test.sh <client-id>
#   ./scripts/copilot-live-test.sh            # reuse GITHUB_COPILOT_AUTH_CLIENT_ID from .env
#
# HUMAN INTERVENTION IS REQUIRED, in two places:
#
#   1. Once, outside this script: register a GitHub App (or OAuth App) with
#      "Enable Device Flow" ticked and install it on your account. Copy its Client ID.
#      GitHub Apps give an Iv23li... id; OAuth Apps give Ov23li...
#
#   2. Every time the token expires: this script prints a user code and waits while you
#      approve it in a browser. GitHub App user tokens (ghu_) last about 8 hours; OAuth
#      App tokens (gho_) do not expire by default, so prefer an OAuth App if you want
#      the credential to survive between runs.
#
# This deliberately does not run unattended. The device grant has no non-interactive
# form, so there is nothing to automate away.
#
# Unlike the other suites this bypasses `make test-core`, which cannot pass Go build
# tags. The OAuth path dispatches through the in-process SDK and does not link without
# -tags copilot_inprocess, so the make target would fail on environment rather than
# behaviour. The test entrypoint is the same one make would invoke.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

ENV_FILE="${ENV_FILE:-.env}"
CLI_VERSION="${COPILOT_CLI_VERSION:-1.0.83}"
DEVICE_CODE_URL="https://github.com/login/device/code"
ACCESS_TOKEN_URL="https://github.com/login/oauth/access_token"

for tool in curl jq go; do
	command -v "$tool" >/dev/null || {
		echo "missing required tool: $tool" >&2
		exit 1
	}
done

# shellcheck disable=SC1090
[[ -f "$ENV_FILE" ]] && set -a && source "$ENV_FILE" && set +a

CLIENT_ID="${1:-${GITHUB_COPILOT_AUTH_CLIENT_ID:-}}"
if [[ -z "$CLIENT_ID" ]]; then
	echo "usage: $0 <client-id>   (or set GITHUB_COPILOT_AUTH_CLIENT_ID in $ENV_FILE)" >&2
	exit 2
fi

# --- Reuse a live token rather than burning a browser round-trip on every run ----
token_is_live() {
	local token="$1"
	[[ -n "$token" ]] || return 1
	[[ "$(curl -sS -o /dev/null -w '%{http_code}' --max-time 10 \
		-H "Authorization: Bearer $token" https://api.github.com/user)" == "200" ]]
}

TOKEN="${GITHUB_COPILOT_OAUTH_TOKEN:-}"
if token_is_live "$TOKEN"; then
	echo "Reusing the saved token (still valid)."
else
	[[ -n "$TOKEN" ]] && echo "Saved token is expired or revoked; re-authorizing."

	device_response="$(curl -sS -X POST "$DEVICE_CODE_URL" \
		-H "Accept: application/json" -d "client_id=$CLIENT_ID")"

	if ! echo "$device_response" | jq -e '.device_code' >/dev/null 2>&1; then
		echo "device code request failed:" >&2
		echo "$device_response" | jq . >&2 2>/dev/null || echo "$device_response" >&2
		echo "If this says device_flow_disabled, tick 'Enable Device Flow' in the app settings." >&2
		exit 1
	fi

	device_code="$(echo "$device_response" | jq -r '.device_code')"
	interval="$(echo "$device_response" | jq -r '.interval // 5')"
	expires_in="$(echo "$device_response" | jq -r '.expires_in // 900')"

	echo
	echo "  Open : $(echo "$device_response" | jq -r '.verification_uri')"
	echo "  Code : $(echo "$device_response" | jq -r '.user_code')"
	echo
	echo "Waiting for authorization (expires in ${expires_in}s)..."

	TOKEN=""
	deadline=$((SECONDS + expires_in))
	while ((SECONDS < deadline)); do
		sleep "$interval"
		poll="$(curl -sS -X POST "$ACCESS_TOKEN_URL" -H "Accept: application/json" \
			-d "client_id=$CLIENT_ID" -d "device_code=$device_code" \
			-d "grant_type=urn:ietf:params:oauth:grant-type:device_code")"
		case "$(echo "$poll" | jq -r '.error // empty')" in
		"")
			TOKEN="$(echo "$poll" | jq -r '.access_token // empty')"
			[[ -n "$TOKEN" ]] && break
			;;
		authorization_pending) ;;
		slow_down) interval=$((interval + 5)) ;;
		*)
			echo "authorization failed: $(echo "$poll" | jq -r '.error')" >&2
			exit 1
			;;
		esac
	done
	[[ -n "$TOKEN" ]] || {
		echo "timed out waiting for authorization" >&2
		exit 1
	}

	# Persist so reruns inside the token's lifetime skip the browser step. Written with
	# a restrictive umask; .env is gitignored.
	umask 077
	touch "$ENV_FILE"
	chmod 600 "$ENV_FILE"
	for var in GITHUB_COPILOT_OAUTH_TOKEN GITHUB_COPILOT_AUTH_CLIENT_ID; do
		grep -v "^${var}=" "$ENV_FILE" >"$ENV_FILE.tmp" 2>/dev/null || true
		mv "$ENV_FILE.tmp" "$ENV_FILE"
	done
	printf 'GITHUB_COPILOT_OAUTH_TOKEN=%s\n' "$TOKEN" >>"$ENV_FILE"
	printf 'GITHUB_COPILOT_AUTH_CLIENT_ID=%s\n' "$CLIENT_ID" >>"$ENV_FILE"
	chmod 600 "$ENV_FILE"
	echo "Token stored in $ENV_FILE (never printed)."
fi

# --- The native runtime is a build input, not a dependency the linker can find ---
echo
echo "Bundling the Copilot native runtime (CLI $CLI_VERSION)..."
(cd core/providers/githubcopilot && go run github.com/github/copilot-sdk/go/cmd/bundler --cli-version "$CLI_VERSION")

echo
echo "Running the live OAuth suite..."
cd core
GITHUB_COPILOT_OAUTH_TOKEN="$TOKEN" \
	GITHUB_COPILOT_AUTH_CLIENT_ID="$CLIENT_ID" \
	GITHUB_COPILOT_OAUTH_MODEL="${GITHUB_COPILOT_OAUTH_MODEL:-}" \
	go test -tags copilot_inprocess ./providers/githubcopilot/ \
	-run TestGithubCopilot -v -timeout 20m "$@"
