// Package crds carries the platform's CRDs in the operator's image (spec 008): Helm installs a chart's crds/ at the
// first install and never again, so before an upgrade hugr-init applies these - the new release's - itself.
// `make manifests` copies them here from config/crd.
package crds

import (
	"embed"
	"io/fs"
	"sort"
)

//go:embed platform.hugr.io_*.yaml
var files embed.FS

// Manifests are the CRDs' YAML, in name order.
func Manifests() ([][]byte, error) {
	names, err := fs.Glob(files, "platform.hugr.io_*.yaml")
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	var out [][]byte
	for _, name := range names {
		b, err := files.ReadFile(name)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}
