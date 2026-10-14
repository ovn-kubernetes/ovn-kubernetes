// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package util

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
)

func TestValidateNodeVTEPs(t *testing.T) {
	peer := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "peer", Annotations: map[string]string{
		OVNNodeVTEPs: `{"other-vtep":{"ips":["100.64.0.2","fd00::2"]}}`,
	}}}
	peer.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeExternalIP, Address: "192.0.2.2"}}
	tests := []struct {
		name      string
		ips       []string
		want      []string
		wantError string
	}{
		{name: "canonicalizes dual stack", ips: []string{"100.64.0.1", "fd00:0:0:0::1"}, want: []string{"100.64.0.1", "fd00::1"}},
		{name: "canonicalizes IPv4 mapped IPv6", ips: []string{"::ffff:100.64.0.1"}, want: []string{"100.64.0.1"}},
		{name: "rejects extra IPv4", ips: []string{"100.64.0.1", "100.64.0.99"}, wantError: "multiple IPs"},
		{name: "rejects extra IPv6", ips: []string{"fd00::1", "fd00::99"}, wantError: "multiple IPs"},
		{name: "rejects mapped extra IPv4", ips: []string{"100.64.0.1", "::ffff:100.64.0.99"}, wantError: "multiple IPs"},
		{name: "rejects conflicting IPv4 mapped IPv6", ips: []string{"::ffff:100.64.0.2"}, wantError: "already claimed"},
		{name: "rejects another VTEP's address", ips: []string{"100.64.0.2"}, wantError: "already claimed"},
		{name: "rejects another node's external address", ips: []string{"192.0.2.2"}, wantError: "already claimed"},
		{name: "rejects IPv6 alternate notation conflict", ips: []string{"fd00:0:0:0::2"}, wantError: "already claimed"},
		{name: "rejects empty address", ips: []string{""}, wantError: "invalid IP"},
		{name: "rejects CIDR", ips: []string{"100.64.0.1/32"}, wantError: "invalid IP"},
		{name: "rejects config text", ips: []string{"100.64.0.1\nrouter bgp 65000"}, wantError: "invalid IP"},
		{name: "rejects unspecified address", ips: []string{"0.0.0.0"}, wantError: "invalid IP"},
		{name: "rejects loopback", ips: []string{"::1"}, wantError: "invalid IP"},
		{name: "rejects multicast", ips: []string{"ff00::1"}, wantError: "invalid IP"},
		{name: "rejects scoped IPv6", ips: []string{"fe80::1%eth0"}, wantError: "invalid IP"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := json.Marshal(map[string]VTEPNodeAnnotation{"vtep": {IPs: tt.ips}})
			if err != nil {
				t.Fatal(err)
			}
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node", Annotations: map[string]string{OVNNodeVTEPs: string(raw)}}}
			node.Status.Addresses = []corev1.NodeAddress{{Type: corev1.NodeInternalIP, Address: "100.64.0.1"}}
			vteps, err := ValidateNodeVTEPs(node, GetNodeVTEPIPOwners([]*corev1.Node{node, peer}))
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("expected error containing %q, got %v", tt.wantError, err)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(vteps["vtep"].IPs, tt.want) {
				t.Fatalf("got %v, %v; want %v", vteps, err, tt.want)
			}
		})
	}
}

func TestParseNodeVTEPs(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		want        map[string]VTEPNodeAnnotation
		wantErr     error
	}{
		{
			name:        "missing annotation",
			annotations: map[string]string{},
			wantErr:     &annotationNotSetError{},
		},
		{
			name:        "invalid JSON",
			annotations: map[string]string{OVNNodeVTEPs: "not-json"},
			wantErr:     &json.SyntaxError{},
		},
		{
			name:        "empty map",
			annotations: map[string]string{OVNNodeVTEPs: "{}"},
			want:        map[string]VTEPNodeAnnotation{},
		},
		{
			name:        "single VTEP with one IP",
			annotations: map[string]string{OVNNodeVTEPs: `{"vtep-a": {"ips": ["100.64.0.1"]}}`},
			want: map[string]VTEPNodeAnnotation{
				"vtep-a": {IPs: []string{"100.64.0.1"}},
			},
		},
		{
			name:        "single VTEP with dual-stack IPs",
			annotations: map[string]string{OVNNodeVTEPs: `{"vtep-a": {"ips": ["100.64.0.1", "fd00::1"]}}`},
			want: map[string]VTEPNodeAnnotation{
				"vtep-a": {IPs: []string{"100.64.0.1", "fd00::1"}},
			},
		},
		{
			name: "multiple VTEPs",
			annotations: map[string]string{
				OVNNodeVTEPs: `{"vtep-a": {"ips": ["100.64.0.1"]}, "vtep-b": {"ips": ["10.0.0.1", "fd00::2"]}}`,
			},
			want: map[string]VTEPNodeAnnotation{
				"vtep-a": {IPs: []string{"100.64.0.1"}},
				"vtep-b": {IPs: []string{"10.0.0.1", "fd00::2"}},
			},
		},
		{
			name:        "VTEP with empty IPs",
			annotations: map[string]string{OVNNodeVTEPs: `{"vtep-a": {"ips": []}}`},
			want: map[string]VTEPNodeAnnotation{
				"vtep-a": {IPs: []string{}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "node1",
					Annotations: tt.annotations,
				},
			}
			got, err := ParseNodeVTEPs(node)
			if tt.wantErr != nil {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				target := reflect.New(reflect.TypeOf(tt.wantErr)).Interface()
				if !errors.As(err, target) {
					t.Fatalf("expected error type %T, got: %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %d VTEPs, want %d", len(got), len(tt.want))
			}
			for name, wantEntry := range tt.want {
				gotEntry, ok := got[name]
				if !ok {
					t.Errorf("missing VTEP %q", name)
					continue
				}
				if len(gotEntry.IPs) != len(wantEntry.IPs) {
					t.Errorf("VTEP %q: got %d IPs, want %d", name, len(gotEntry.IPs), len(wantEntry.IPs))
					continue
				}
				for i := range wantEntry.IPs {
					if gotEntry.IPs[i] != wantEntry.IPs[i] {
						t.Errorf("VTEP %q IP[%d]: got %q, want %q", name, i, gotEntry.IPs[i], wantEntry.IPs[i])
					}
				}
			}
		})
	}
}

func TestValidateSelectedNodeVTEPs(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node", Annotations: map[string]string{
		OVNNodeVTEPs: `{"selected":{"ips":["fd00:0:0:0::1"]},"stale":{"ips":["100.64.0.2"]}}`,
	}}}
	owners := map[string]sets.Set[string]{"100.64.0.2": sets.New("peer")}
	for _, tt := range []struct {
		name      string
		selected  sets.Set[string]
		want      map[string]VTEPNodeAnnotation
		wantError bool
	}{
		{name: "ignores unselected ownership conflict", selected: sets.New("selected"), want: map[string]VTEPNodeAnnotation{"selected": {IPs: []string{"fd00::1"}}}},
		{name: "rejects selected ownership conflict", selected: sets.New("stale"), wantError: true},
		{name: "nil validates all entries", wantError: true},
		{name: "empty selects nothing", selected: sets.New[string](), want: map[string]VTEPNodeAnnotation{}},
		{name: "missing selection returns no entries", selected: sets.New("missing"), want: map[string]VTEPNodeAnnotation{}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ValidateSelectedNodeVTEPs(node, owners, tt.selected)
			if (err != nil) != tt.wantError {
				t.Fatalf("unexpected error: %v", err)
			}
			if !tt.wantError && !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}
