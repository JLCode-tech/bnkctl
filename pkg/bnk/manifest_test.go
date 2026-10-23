package bnk

import (
	"context"
	"testing"
)

func TestProbe_Embedded(t *testing.T) {
	m, err := Probe(context.Background(), "", "")
	if err != nil {
		t.Fatalf("Probe failed: %v", err)
	}
	if m.Version != DefaultManifestVersion {
		t.Fatalf("expected version %s, got %s", DefaultManifestVersion, m.Version)
	}
	if m.FLOVersion == "" {
		t.Fatal("expected non-empty FLOVersion")
	}
	if len(m.HelmCharts) == 0 {
		t.Fatal("expected helm charts, got 0")
	}
}

func TestFLOChartFor(t *testing.T) {
	flo, ok := FLOChartFor("2.4.0")
	if !ok || flo != "v2.30.0-0.5.2" {
		t.Fatalf("FLOChartFor(2.4.0) = %q, %v; want v2.30.0-0.5.2, true", flo, ok)
	}
	flo23, ok := FLOChartFor("2.3.0-3.2598.3-0.0.170")
	if !ok || flo23 != "v2.21.13-0.0.28" {
		t.Fatalf("FLOChartFor(2.3.0) = %q, %v; want v2.21.13-0.0.28, true", flo23, ok)
	}
}
