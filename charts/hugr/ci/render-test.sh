#!/usr/bin/env bash
# The umbrella's render tests (spec 005): what its values wire into the services' configs, and what its checks refuse.
# Run by `make chart` after `helm dependency build charts/hugr`.
set -euo pipefail
CHART="$(cd "$(dirname "$0")/.." && pwd)"
EXAMPLE="$CHART/ci/example-values.yaml"
OUT="$(mktemp)"
trap 'rm -f "$OUT"' EXIT

render() { helm template hugr "$CHART" -n hugr -f "$EXAMPLE" "$@"; }
fail() {
	echo "FAIL: $*" >&2
	exit 1
}
# renders <what> <pattern> [helm args]: the render holds the line
renders() {
	local what="$1" pattern="$2"
	shift 2
	render "$@" >"$OUT" || fail "$what: the render failed"
	grep -Eq -- "$pattern" "$OUT" || fail "$what: no line matches $pattern"
	echo "ok: $what"
}
# refuses <what> <message> [helm args]: the render fails, naming why
refuses() {
	local what="$1" message="$2"
	shift 2
	if render "$@" >"$OUT" 2>&1; then
		fail "$what: rendered"
	fi
	grep -Fq -- "$message" "$OUT" || fail "$what: refused, but not with \"$message\": $(tail -1 "$OUT")"
	echo "ok: $what refused"
}

renders "tresor serves https on 443" 'port: 443, targetPort: http'
renders "tresor's KEK on the Transit mount" '^      mount: transit$'
renders "a release version is kista's release kind" '^ +- kind: release$'
renders "a commit's version is kista's dev kind" '^ +- kind: dev$' --set global.duckdbVersion=f8f3f4dcd9
renders "a private IdP's CA is tresor's for that issuer" 'ca_file: /var/run/tresor/cas/idp.crt' \
	--set-string global.idp.caCert=PEM
renders "the IdP's groups claim reaches tresor" 'groups_claim: groups' --set global.idp.tresorGroupsClaim=groups
renders "the private CAs reach the platform's bundle" 'hugr-private-cas/idp.crt'
# the databases Job (not the platform's hugr-init) runs on every upgrade, after the catalog phase deleted the Secret
render --show-only templates/databases.yaml >"$OUT" || fail "the databases Job: the render failed"
grep -A1 -- '- name: db-admin' "$OUT" | grep -q 'optional: true' ||
	fail "the databases Job mounts the administrator's Secret as required: an upgrade's run never starts"
echo "ok: the databases Job runs without the administrator's Secret (an upgrade)"
refuses "an admins list without hugr-init" "must keep hugr-init's entry" --set 'tresor.config.policy.admins={role:x}'
refuses "a tresor namespace other than global's" "tresor.namespaceOverride must be global.namespaces.tresor" \
	--set tresor.namespaceOverride=elsewhere
refuses "a vault audience other than global's" "tresor.vaultToken.audience must be global.keys.vault.audience" \
	--set tresor.vaultToken.audience=other
refuses "a keys mode not built yet" "external is the one built so far" --set global.keys.mode=internal
refuses "a database without a host" "global.database.host is required" --set global.database.host=
echo "PASS: the umbrella's renders"
