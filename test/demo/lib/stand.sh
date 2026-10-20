# shellcheck shell=bash
# The console for a person's browser (spec 003): `run.sh <kind | minikube> console` after an up - Keycloak and the
# console forwarded to this machine on the ports the realm's console client names, until Ctrl-C. The browser reaches
# Keycloak by the issuer's own name (the nodes trust https://keycloak.idp.svc:8443/realms/hugr), so that name resolves
# to 127.0.0.1 here: a line of /etc/hosts, the person's to add.
STAND_KC_PORT=8443
STAND_CONSOLE_PORT=8444

console_stand() {
	[ "$IDP_NAME" = Keycloak ] || fail "the console stand signs a person in at the demo's Keycloak: kind or minikube"
	k -n hugr get deploy hugr-platform >/dev/null 2>&1 || fail "no platform here: run up first"
	[ -s "$WORK/secrets/person-operator" ] || fail "no console person in this run's realm: run up again"
	step "the console for a browser (spec 003)"
	provider_metrics
	local port
	for port in $STAND_KC_PORT $STAND_CONSOLE_PORT; do
		! (echo >"/dev/tcp/127.0.0.1/$port") 2>/dev/null || fail "port $port is taken on this machine"
	done
	# kubectl itself in the background, never the function k (forward says why); the run's exit stops them
	command kubectl --context "$DEMO_CONTEXT" -n idp port-forward svc/keycloak "$STAND_KC_PORT:8443" \
		</dev/null >"$WORK/stand-keycloak.log" 2>&1 &
	echo "$!" >>"$WORK/forwards.pid"
	command kubectl --context "$DEMO_CONTEXT" -n hugr port-forward svc/hugr-platform-console "$STAND_CONSOLE_PORT:443" \
		</dev/null >"$WORK/stand-console.log" 2>&1 &
	echo "$!" >>"$WORK/forwards.pid"
	eventually 30 "the console answers on this machine" curl -fsS --cacert "$WORK/ca.crt" \
		"https://127.0.0.1:$STAND_CONSOLE_PORT/api/v1/console/config"
	ok "the console: https://127.0.0.1:$STAND_CONSOLE_PORT"
	if ! grep -Eq '^[[:space:]]*127\.0\.0\.1([[:space:]]+[^#[:space:]]+)*[[:space:]]+keycloak\.idp\.svc([[:space:]]|$)' /etc/hosts; then
		echo "  the browser reaches Keycloak as keycloak.idp.svc: add to /etc/hosts (yours to add, once)"
		echo "    127.0.0.1 keycloak.idp.svc"
	fi
	echo "  sign in as operator, the password in $WORK/secrets/person-operator"
	echo "  the run's CA (trust it, or accept the browser's warning for both addresses): $WORK/ca.crt"
	echo "  open https://keycloak.idp.svc:$STAND_KC_PORT/ first if the CA is not trusted; Ctrl-C ends the stand"
	wait
}
