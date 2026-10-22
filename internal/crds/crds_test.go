package crds

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// the image carries what the chart installs: `make manifests` copies both from config/crd
func TestTheSameAsTheChartsCRDs(t *testing.T) {
	manifests, err := Manifests()
	if err != nil {
		t.Fatal(err)
	}
	chart, err := filepath.Glob("../../charts/hugr-platform/crds/platform.hugr.io_*.yaml")
	if err != nil || len(chart) != len(manifests) || len(chart) != 2 {
		t.Fatalf("the image carries %d CRDs, the chart %d (%v)", len(manifests), len(chart), err)
	}
	for i, path := range chart {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(b, manifests[i]) {
			t.Errorf("%s differs from the image's copy: run make manifests", filepath.Base(path))
		}
	}
}
