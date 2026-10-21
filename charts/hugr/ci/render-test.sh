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
# tresor's chart refuses these first (a subchart renders before the umbrella's checks): either refusal holds
refuses "a keys mode not built yet" "config.vault.address is required" --set global.keys.mode=secret
refuses "an external vault without its address" "config.vault.address is required" --set global.keys.mode=external
renders "an external vault is the services' address" '^ +address: https://vault[.]example[.]net:8200$' \
	--set global.keys.mode=external --set global.keys.vault.address=https://vault.example.net:8200
renders "the internal OpenBao is tresor's vault" '^ +address: https://openbao[.]hugr-openbao[.]svc:8200$'
renders "the internal OpenBao's CA is the services' vault CA" '^ +ca_file: /etc/kista/secrets/cas/vault[.]crt$'
renders "the internal OpenBao opens by a static key" 'current_key += "file:///openbao/seal/key"'
renders "the internal OpenBao's standbys forward every request" '^ +disable_standby_reads = true$'
renders "the internal OpenBao has a database of its own" 'secret: [{]namespace: hugr-openbao, name: openbao-db, key: password[}]'
renders "the internal OpenBao's Job binds kista's role to its ServiceAccount" '^ +serviceAccount: "hugr-kista"$'
# OpenBao's CA is a CA of the umbrella's own, the one in tresor's and kista's vault.crt: the same certificate
render --show-only templates/services-ca.yaml >"$OUT" || fail "the CAs: the render failed"
python3 - "$OUT" <<'PY2' || fail "the services' vault.crt is not OpenBao's CA"
import base64, re, sys
text = open(sys.argv[1]).read()
docs = text.split("\n---")
ca = [d for d in docs if "name: openbao-ca" in d][0]
crt = base64.b64decode(re.search(r"tls.crt: (\S+)", ca).group(1)).decode().strip()
vaults = [d for d in docs if "name: hugr-cas" in d and ("namespace: hugr-kista" in d or "namespace: hugr-tresor" in d)]
assert len(vaults) == 2, len(vaults)
for d in vaults:
    v = re.search(r'vault.crt: "((?:[^"\\]|\\.)*)"', d).group(1).encode().decode("unicode_escape").strip()
    assert v == crt, "a vault.crt differs from OpenBao's CA"
PY2
echo "ok: the services' vault.crt is the internal OpenBao's CA"
refuses "a database without a host" "global.database.host is required" --set global.database.host=
echo "PASS: the umbrella's renders"
