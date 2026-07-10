package main

import (
	"testing"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
)

func TestConfigureZone(t *testing.T) {
	originalZone := config.Zone
	t.Cleanup(func() {
		config.Zone = originalZone
	})

	tests := []struct {
		name        string
		initialZone string
		runMode     *ovnkubeRunMode
		wantZone    string
		wantErr     bool
	}{
		{
			name:        "cluster manager does not set zone",
			initialZone: "",
			runMode:     &ovnkubeRunMode{clusterManager: true, identity: "node1"},
			wantZone:    "",
		},
		{
			name:        "node defaults zone to identity",
			initialZone: "",
			runMode:     &ovnkubeRunMode{node: true, identity: "node1"},
			wantZone:    "node1",
		},
		{
			name:        "controller sets zone to identity",
			initialZone: "",
			runMode:     &ovnkubeRunMode{ovnkubeController: true, identity: "node1"},
			wantZone:    "node1",
		},
		{
			name:        "zone scoped mode requires identity",
			initialZone: "",
			runMode:     &ovnkubeRunMode{node: true},
			wantZone:    "",
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config.Zone = tt.initialZone
			err := configureZone(tt.runMode)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected configureZone error")
				}
			} else if err != nil {
				t.Fatalf("configureZone returned unexpected error: %v", err)
			}
			if config.Zone != tt.wantZone {
				t.Fatalf("config.Zone = %q, want %q", config.Zone, tt.wantZone)
			}
		})
	}
}
