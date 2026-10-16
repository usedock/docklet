package main

import (
	"reflect"
	"testing"

	apiv1 "github.com/usedock/docklet/api/v1"
)

func TestRegisterFeatures(t *testing.T) {
	cases := []struct {
		egress, microVM bool
		want            []string
	}{
		{true, true, []string{"egress-on-resume", apiv1.FeatureVerifiedImports}},
		{false, true, []string{apiv1.FeatureVerifiedImports}},
		// gVisor hosts keep only the workspace: no uploaded disks, but they export the work dir.
		{true, false, []string{"egress-on-resume", apiv1.FeatureWorkExport}},
		{false, false, []string{apiv1.FeatureWorkExport}},
	}
	for _, c := range cases {
		if got := registerFeatures(c.egress, c.microVM); !reflect.DeepEqual(got, c.want) {
			t.Errorf("registerFeatures(%v, %v) = %v, want %v", c.egress, c.microVM, got, c.want)
		}
	}
	if apiv1.FeatureWorkExport != "work-export" {
		t.Fatalf("the control plane matches the feature by name: %q", apiv1.FeatureWorkExport)
	}
	if apiv1.FeatureVerifiedImports != "verified-imports" {
		t.Fatalf("the control plane matches the feature by name: %q", apiv1.FeatureVerifiedImports)
	}
}
