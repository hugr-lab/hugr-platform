#!/usr/bin/env bash
# The umbrella's render tests (spec 005): what its values wire into the services' configs, and what its checks refuse.
# Run by `make chart` after `helm dependency build charts/hugr`.
set -euo pipefail
CHART="$(cd "$(dirname "$0")/.." && pwd)"
EXAMPLE="$CHART/ci/example-values.yaml"
OWN="$CHART/ci/own-values.yaml"
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

# --- the default: every part given by its address ---------------------------------------------------------------------
render >"$OUT" || fail "the external parts: the render failed"
! grep -Eq '^kind: (Namespace|StatefulSet)$' "$OUT" && ! grep -q 'hugr-init-secrets' "$OUT" ||
	fail "given every part, the umbrella runs one of its own"
echo "ok: given every part, the umbrella runs none of its own"
renders "the given tresor is the platform's" '^    host: tresor[.]example[.]net:443$'
renders "the given channel is the platform's repository" '^    prefix: https://kista[.]example[.]net/hugr/platform$'
renders "hugr-init writes into the given tresor" 'url: "https://tresor[.]example[.]net:443"'
renders "hugr-init makes the catalog on the given server" 'host: "hugr-pg[.]example[.]net"'
renders "the given parts' private CAs reach the platform's bundle" 'hugr-private-cas/repository[.]crt'
refuses "a tresor neither given nor its own" "global.tresor.host is required" --set global.tresor.host=
refuses "a channel neither given nor its own" "global.repository.prefix is required" --set global.repository.prefix=
refuses "a database neither given nor its own" "global.database.host is required" --set global.database.host=
refuses "a tresor given and its own" "install.tresor runs the umbrella's own" --set install.tresor=true
refuses "a tresor host with a scheme" "host[:port], no scheme" --set global.tresor.host=https://tresor.example.net
refuses "a channel over http" "the channel's https prefix" --set global.repository.prefix=http://kista.example.net/x
# --- its own parts --------------------------------------------------------------------------------------------------
own() { renders "$@" -f "$OWN"; }
own "its own tresor serves https on 443" 'port: 443, targetPort: http'
own "its own tresor keeps its state on SQLite" '^      kind: sqlite$'
own "its own tresor's KEK is its Secret" 'key_file: /var/run/tresor/kek/key'
own "a private IdP's CA is its own tresor's for that issuer" 'ca_file: /var/run/tresor/cas/idp.crt' \
	--set-string global.idp.caCert=PEM
own "the IdP's groups claim reaches its own tresor" 'groups_claim: groups' --set global.idp.tresorGroupsClaim=groups
own "its own kista signs with the key in its Secret" '^        kind: kubesecret$'
own "its own kista's channel names that key" 'key: keys:channel'
own "a release version is its own kista's release kind" '^ +- kind: release$'
own "a commit's version is its own kista's dev kind" '^ +- kind: dev$' --set global.duckdbVersion=f8f3f4dcd9
own "its own database is a PostgreSQL 18 pod" 'image: "postgres:18"'
own "its own database serves TLS" 'ssl_cert_file=/var/run/postgres/tls/tls.crt'
own "hugr-init makes the catalog on its own database" 'host: "postgres[.]hugr-database[.]svc"'
own "the secrets phase runs for its own parts" 'command: \[/hugr-init, --phase, secrets'
refuses "its own kista without the installer's statement" "global.etcdEncryptionAtRest" -f "$OWN" \
	--set global.etcdEncryptionAtRest=false
refuses "an admins list without hugr-init" "must keep hugr-init's entry" -f "$OWN" \
	--set 'tresor.config.policy.admins={role:x}'
refuses "a tresor namespace other than global's" "tresor.namespaceOverride must be global.namespaces.tresor" -f "$OWN" \
	--set tresor.namespaceOverride=elsewhere
# the secrets phase's right: each part's own Secret by name, and the database's administrator copy
render -f "$OWN" >"$OUT" || fail "the own parts: the render failed"
python3 - "$OUT" <<'PY2' || fail "the secrets phase's rights"
import sys
docs = open(sys.argv[1]).read().split("\n---")
roles = {d.split("namespace: ")[1].split("\n")[0]: d for d in docs if "kind: Role\n" in d and "name: hugr-init-secrets" in d}
assert set(roles) == {"hugr", "hugr-tresor", "hugr-kista", "hugr-database"}, sorted(roles)
for ns, name in [("hugr-tresor", "tresor-kek"), ("hugr-kista", "kista-channel-key"), ("hugr-database", "postgres-superuser"),
                 ("hugr", "hugr-init-db-admin")]:
    assert f"resourceNames: [{name}]" in roles[ns], (ns, name)
assert "verbs: [get, update]" in roles["hugr"], "the administrator's copy"
PY2
echo "ok: the secrets phase may read only its own Secrets by name in each part's namespace"
# spec 008: the CRDs applied before an upgrade, by a hook whose right is the two CRDs only
render >"$OUT"
python3 - "$OUT" <<'PY3' || fail "the CRDs' pre-upgrade hook"
import re, sys
docs = sys.argv[1] and open(sys.argv[1]).read().split("\n---")
job = [d for d in docs if "kind: Job" in d and "name: hugr-crds" in d]
assert len(job) == 1 and "helm.sh/hook: pre-upgrade" in job[0] and "--phase, crds" in job[0], "the Job"
role = [d for d in docs if "kind: ClusterRole\n" in d and "hugr-crds" in d]
assert len(role) == 1, "the ClusterRole"
assert "resourceNames: [hugrplatforms.platform.hugr.io, hugrnodegroups.platform.hugr.io]" in role[0], "its names"
assert "verbs: [get, patch]" in role[0] and role[0].count("verbs:") == 1, "its verbs"
PY3
echo "ok: the CRDs are applied before an upgrade by a hook whose right is the two CRDs only"
echo "PASS: the umbrella's renders"
