# shellcheck shell=bash
# The demo's images (spec 004): the operator from this checkout; hugr-node the release the umbrella chart pins - its
# published image and its extensions, moved by hand with the chart. Each is <name>:demo, built by the provider: on kind
# and minikube locally and loaded into the cluster, on AKS in the run's registry. tresor-server and kista are the
# releases the umbrella chart pins, their published images (spec 005).

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

# src <repository> <var> <tag>: a checkout of hugr-lab/<repository> at the tag in the cache (or the directory the
# caller names)
src() {
	local repo="$1" var="$2" tag="$3" dir
	dir="${!var:-}"
	if [ -n "$dir" ]; then
		(cd "$dir" && pwd)
		return
	fi
	dir="$CACHE/src/$repo@$tag"
	if [ ! -d "$dir/.git" ]; then
		mkdir -p "$CACHE/src"
		git clone -q --depth 1 --branch "$tag" "https://github.com/hugr-lab/$repo" "$dir" 2>/dev/null ||
			fail "hugr-lab/$repo at $tag"
	fi
	echo "$dir"
}

# node_version: the hugr-node release the umbrella chart pins (its node image's tag)
node_version() {
	local v
	v="$(sed -n 's|^ *image: ghcr\.io/hugr-lab/hugr-node:\(v[0-9][0-9A-Za-z.-]*\)$|\1|p' "$ROOT/charts/hugr/values.yaml")"
	[ -n "$v" ] && [ "$(echo "$v" | wc -l)" -eq 1 ] || fail "no single hugr-node image in charts/hugr/values.yaml"
	echo "$v"
}

# node_dist: the node's assets for the cluster's architecture - HUGR_NODE_DIST, or the archive of the pinned release
# (hugr-node-<tag>-linux_<arch>.tar.gz), checked against the release's SHA256SUMS
node_dist() {
	if [ -n "${HUGR_NODE_DIST:-}" ]; then
		(cd "$HUGR_NODE_DIST" && pwd)
		return
	fi
	local name="hugr-node-$NODE_VERSION-linux_$ARCH.tar.gz" dir="$CACHE/hugr-node-$NODE_VERSION-linux_$ARCH" get
	get="$WORK/node-release"
	if [ ! -f "$dir/.done" ]; then
		rm -rf "$dir" "$get" && mkdir -p "$dir" "$get"
		for f in "$name" SHA256SUMS; do
			curl -fsSL -o "$get/$f" "https://github.com/hugr-lab/hugr-node/releases/download/$NODE_VERSION/$f" ||
				fail "hugr-node $NODE_VERSION's $f"
		done
		(cd "$get" && grep -E "^[0-9a-f]{64}  $name\$" SHA256SUMS | shasum -a 256 -c --status -) ||
			fail "hugr-node $NODE_VERSION's $name does not match its SHA256SUMS"
		tar -xzf "$get/$name" -C "$dir" || fail "unpacking $name"
		touch "$dir/.done"
	fi
	echo "$dir"
}

build_images() {
	step "the images"
	provider_build hugr-platform "$ROOT"
	ok "the operator's image $(img hugr-platform), from this checkout"
	NODE_VERSION="$(node_version)"
	NODE_SRC="$(src hugr-node HUGR_NODE_DIR "$NODE_VERSION")"
	# the assets seed kista whichever image runs: the node's extensions are its release's, for its DuckDB
	DIST="$(node_dist)"
	DUCKDB_VERSION="$(python3 -c 'import json,sys; print(json.loads(sys.argv[1])["duckdb_version"])' \
		"$(python3 "$NODE_SRC/scripts/ext_footer.py" "$DIST/hugr_node.duckdb_extension")")" ||
		fail "the node's DuckDB version"
	# the pinned release's image, unless the assets are one's own (HUGR_NODE_DIST): then the image is built from them
	[ -n "${HUGR_NODE_DIST:-}" ] || NODE_IMAGE="${NODE_IMAGE:-ghcr.io/hugr-lab/hugr-node:$NODE_VERSION}"
	if [ -n "${NODE_IMAGE:-}" ]; then
		provider_import "$NODE_IMAGE" hugr-node
		ok "$(img hugr-node), from $NODE_IMAGE"
	else
		# the context is the node's binary and the release's Dockerfile, nothing else
		rm -rf "$WORK/node-image" && mkdir -p "$WORK/node-image"
		cp "$DIST/duckdb" "$WORK/node-image/duckdb"
		cp "$NODE_SRC/docker/Dockerfile" "$WORK/node-image/Dockerfile"
		provider_build hugr-node "$WORK/node-image"
		ok "$(img hugr-node), from the assets in $DIST"
	fi
}
