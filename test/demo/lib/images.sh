# shellcheck shell=bash
# The demo's images (spec 004): the operator from this checkout; hugr-node from the assets of a hugr-node release dry
# run (dist-linux_<arch>) - nothing new is published. Each is <name>:demo, built by the provider: on kind and minikube
# locally and loaded into the cluster, on AKS in the run's registry. tresor-server and kista are the releases the
# umbrella chart pins, their published images (spec 005).

# REGISTRY: the images' registry with its slash (AKS's), or none (loaded into the cluster); PULL: their pull policy
REGISTRY=""
PULL=Never
img_repo() { echo "$REGISTRY$1"; }
img() { echo "$REGISTRY$1:demo"; }

# local_import <image> <name>: a published image as <name>:demo, loaded into the cluster (kind, minikube)
local_import() {
	docker image inspect "$1" >/dev/null 2>&1 || docker pull -q "$1" >/dev/null || fail "no $1"
	docker tag "$1" "$2:demo"
	provider_load "$2:demo"
}

# local_build <name> <context>: built here, loaded into the cluster (kind, minikube)
local_build() {
	docker build -q -t "$1:demo" --build-arg VERSION=demo "$2" >/dev/null || fail "the image $1"
	provider_load "$1:demo"
}

# src <repository>: a checkout of hugr-lab/<repository>'s main in the cache (or the directory the caller names)
src() {
	local repo="$1" var="$2" dir
	dir="${!var:-}"
	if [ -n "$dir" ]; then
		(cd "$dir" && pwd)
		return
	fi
	dir="$CACHE/src/$repo"
	if [ -d "$dir/.git" ]; then
		git -C "$dir" fetch -q --depth 1 origin main && git -C "$dir" reset -q --hard origin/main
	else
		mkdir -p "$CACHE/src"
		git clone -q --depth 1 "https://github.com/hugr-lab/$repo" "$dir"
	fi
	echo "$dir"
}

# node_dist: the node's assets for the cluster's architecture - HUGR_NODE_DIST, or the artifact of hugr-node's latest
# successful release dry run on main that has one
node_dist() {
	if [ -n "${HUGR_NODE_DIST:-}" ]; then
		(cd "$HUGR_NODE_DIST" && pwd)
		return
	fi
	local name="dist-linux_$ARCH" dir run runs
	dir="$CACHE/$name"
	# gh's own refusal (a token that may not read hugr-node's runs) is said as such, not as "no run"
	runs="$(gh run list -R hugr-lab/hugr-node --workflow release.yml --branch main --status success --limit 20 \
		--json databaseId --jq '.[].databaseId' 2>"$WORK/gh.err")" || fail "listing hugr-node's runs: $(cat "$WORK/gh.err")"
	run="$(echo "$runs" | while read -r id; do
		gh api "repos/hugr-lab/hugr-node/actions/runs/$id/artifacts" --jq '.artifacts[] | select(.expired | not) | .name' |
			grep -qx "$name" && { echo "$id"; break; }
	done)"
	[ -n "$run" ] || fail "no hugr-node release run with $name (dispatch hugr-node's release workflow on main)"
	if [ ! -f "$dir/.run" ] || [ "$(cat "$dir/.run")" != "$run" ]; then
		rm -rf "$dir"
		gh run download "$run" -R hugr-lab/hugr-node -n "$name" -D "$dir" >/dev/null || fail "the artifact $name of run $run"
		echo "$run" >"$dir/.run"
	fi
	echo "$dir"
}

build_images() {
	step "the images"
	provider_build hugr-platform "$ROOT"
	ok "the operator's image $(img hugr-platform), from this checkout"
	NODE_SRC="$(src hugr-node HUGR_NODE_DIR)"
	# the assets seed kista whichever image runs: the node's extensions are its release's, for its DuckDB
	DIST="$(node_dist)"
	DUCKDB_VERSION="$(python3 -c 'import json,sys; print(json.loads(sys.argv[1])["duckdb_version"])' \
		"$(python3 "$NODE_SRC/scripts/ext_footer.py" "$DIST/hugr_node.duckdb_extension")")" ||
		fail "the node's DuckDB version"
	if [ -n "${NODE_IMAGE:-}" ]; then
		provider_import "$NODE_IMAGE" hugr-node
		ok "$(img hugr-node), from $NODE_IMAGE"
	else
		# the context is the node's binary and the release's Dockerfile, nothing else
		rm -rf "$WORK/node-image" && mkdir -p "$WORK/node-image"
		cp "$DIST/duckdb" "$WORK/node-image/duckdb"
		cp "$NODE_SRC/docker/Dockerfile" "$WORK/node-image/Dockerfile"
		provider_build hugr-node "$WORK/node-image"
		ok "$(img hugr-node), from the release dry run assets in $DIST"
	fi
}
