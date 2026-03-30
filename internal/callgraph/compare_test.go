package callgraph

import (
	"testing"
)

func TestComputePruneRatios(t *testing.T) {
	stats := []InterfaceEdgeStats{
		{CHAEdges: 100, CHAOnly: 95},
		{CHAEdges: 0, CHAOnly: 0},
	}
	computePruneRatios(stats)
	if stats[0].PruneRatio != 0.95 {
		t.Errorf("PruneRatio = %v, want 0.95", stats[0].PruneRatio)
	}
}

func TestPruneTargets(t *testing.T) {
	stats := []InterfaceEdgeStats{
		{Interface: "io.Reader", Method: "Read", CHAEdges: 1000, CHAOnly: 990},
		{Interface: "proto.Message", Method: "ProtoReflect", CHAEdges: 100, CHAOnly: 66},
		{Interface: "small", Method: "X", CHAEdges: 50, CHAOnly: 49},
	}
	computePruneRatios(stats)

	keys := PruneTargets(stats, 0.95, 100)
	if !keys["io.Reader\x00Read"] {
		t.Error("expected io.Reader.Read to be a prune target")
	}
	if keys["proto.Message\x00ProtoReflect"] {
		t.Error("66% prune ratio should not be a target at 0.95 threshold")
	}
	if keys["small\x00X"] {
		t.Error("CHAOnly 49 < minExcess 100 should not be a target")
	}
}
