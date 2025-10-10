package e2e

import (
	"context"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// Mock clientsets for testing IP family filtering
func mockIPv4OnlyClient() *fake.Clientset {
	client := fake.NewSimpleClientset()
	// Create a node with IPv4 address only
	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-node",
		},
		Status: v1.NodeStatus{
			Addresses: []v1.NodeAddress{
				{
					Type:    v1.NodeInternalIP,
					Address: "10.0.0.1",
				},
			},
			Conditions: []v1.NodeCondition{
				{
					Type:   v1.NodeReady,
					Status: v1.ConditionTrue,
				},
			},
		},
	}
	client.CoreV1().Nodes().Create(context.TODO(), node, metav1.CreateOptions{})
	return client
}

func mockIPv6OnlyClient() *fake.Clientset {
	client := fake.NewSimpleClientset()
	// Create a node with IPv6 address only
	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-node",
		},
		Status: v1.NodeStatus{
			Addresses: []v1.NodeAddress{
				{
					Type:    v1.NodeInternalIP,
					Address: "fd00::1",
				},
			},
			Conditions: []v1.NodeCondition{
				{
					Type:   v1.NodeReady,
					Status: v1.ConditionTrue,
				},
			},
		},
	}
	client.CoreV1().Nodes().Create(context.TODO(), node, metav1.CreateOptions{})
	return client
}

func mockDualStackClient() *fake.Clientset {
	client := fake.NewSimpleClientset()
	// Create a node with both IPv4 and IPv6 addresses
	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-node",
		},
		Status: v1.NodeStatus{
			Addresses: []v1.NodeAddress{
				{
					Type:    v1.NodeInternalIP,
					Address: "10.0.0.1",
				},
				{
					Type:    v1.NodeInternalIP,
					Address: "fd00::1",
				},
			},
			Conditions: []v1.NodeCondition{
				{
					Type:   v1.NodeReady,
					Status: v1.ConditionTrue,
				},
			},
		},
	}
	client.CoreV1().Nodes().Create(context.TODO(), node, metav1.CreateOptions{})
	return client
}

// TestFilterCIDRsAndJoin_WithDoubleSlashFormat tests the handling of double-slash CIDR format
// used in layer3 topology (e.g., "172.31.0.0/16/24")
func TestFilterCIDRsAndJoin_WithDoubleSlashFormat(t *testing.T) {
	tests := []struct {
		name     string
		client   *fake.Clientset
		input    string
		expected string
	}{
		{
			name:     "IPv4 only cluster filters out IPv6 double-slash CIDR",
			client:   mockIPv4OnlyClient(),
			input:    "172.31.0.0/16/24,2010:100:200::0/60/64",
			expected: "172.31.0.0/16",
		},
		{
			name:     "IPv6 only cluster filters out IPv4 double-slash CIDR",
			client:   mockIPv6OnlyClient(),
			input:    "172.31.0.0/16/24,2010:100:200::0/60/64",
			expected: "2010:100:200::0/60",
		},
		{
			name:     "Dual-stack cluster accepts both double-slash CIDRs",
			client:   mockDualStackClient(),
			input:    "172.31.0.0/16/24,2010:100:200::0/60/64",
			expected: "172.31.0.0/16,2010:100:200::0/60",
		},
		{
			name:     "Single IPv4 double-slash CIDR",
			client:   mockIPv4OnlyClient(),
			input:    "172.31.0.0/16/24",
			expected: "172.31.0.0/16",
		},
		{
			name:     "Single IPv6 double-slash CIDR",
			client:   mockIPv6OnlyClient(),
			input:    "2010:100:200::0/60/64",
			expected: "2010:100:200::0/60",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := filterCIDRsAndJoin(tt.client, tt.input)
			if result != tt.expected {
				t.Errorf("filterCIDRsAndJoin(%q) = %q, expected %q", tt.input, result, tt.expected)
			}
		})
	}
}

// TestFilterCIDRsAndJoin_WithSingleSlashFormat tests backward compatibility
// with standard single-slash CIDR format used in layer2 and localnet topologies
func TestFilterCIDRsAndJoin_WithSingleSlashFormat(t *testing.T) {
	tests := []struct {
		name     string
		client   *fake.Clientset
		input    string
		expected string
	}{
		{
			name:     "IPv4 only cluster filters out IPv6 standard CIDR",
			client:   mockIPv4OnlyClient(),
			input:    "172.31.0.0/24,2010:100:200::0/60",
			expected: "172.31.0.0/24",
		},
		{
			name:     "IPv6 only cluster filters out IPv4 standard CIDR",
			client:   mockIPv6OnlyClient(),
			input:    "172.31.0.0/24,2010:100:200::0/60",
			expected: "2010:100:200::0/60",
		},
		{
			name:     "Dual-stack cluster accepts both standard CIDRs",
			client:   mockDualStackClient(),
			input:    "172.31.0.0/24,2010:100:200::0/60",
			expected: "172.31.0.0/24,2010:100:200::0/60",
		},
		{
			name:     "Layer2 CIDR format",
			client:   mockIPv4OnlyClient(),
			input:    "172.31.0.0/24,2010:100:200::0/60",
			expected: "172.31.0.0/24",
		},
		{
			name:     "Localnet CIDR format",
			client:   mockIPv4OnlyClient(),
			input:    "60.128.0.0/24,2010:100:200::0/60",
			expected: "60.128.0.0/24",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := filterCIDRsAndJoin(tt.client, tt.input)
			if result != tt.expected {
				t.Errorf("filterCIDRsAndJoin(%q) = %q, expected %q", tt.input, result, tt.expected)
			}
		})
	}
}

// TestFilterCIDRsAndJoin_EdgeCases tests edge cases and error conditions
func TestFilterCIDRsAndJoin_EdgeCases(t *testing.T) {
	client := mockIPv4OnlyClient()

	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "Empty string",
			input:    "",
			expected: "",
		},
		{
			name:     "Whitespace only",
			input:    "   ",
			expected: "",
		},
		{
			name:     "Single comma",
			input:    ",",
			expected: "",
		},
		{
			name:     "Trailing comma",
			input:    "172.31.0.0/24,",
			expected: "172.31.0.0/24",
		},
		{
			name:     "Leading comma",
			input:    ",172.31.0.0/24",
			expected: "172.31.0.0/24",
		},
		{
			name:     "Multiple consecutive commas",
			input:    "172.31.0.0/24,,2010:100:200::0/60",
			expected: "172.31.0.0/24",
		},
		{
			name:     "CIDRs with spaces",
			input:    " 172.31.0.0/24 , 2010:100:200::0/60 ",
			expected: "172.31.0.0/24",
		},
		{
			name:     "Double-slash with spaces",
			input:    " 172.31.0.0/16/24 , 2010:100:200::0/60/64 ",
			expected: "172.31.0.0/16",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := filterCIDRsAndJoin(client, tt.input)
			if result != tt.expected {
				t.Errorf("filterCIDRsAndJoin(%q) = %q, expected %q", tt.input, result, tt.expected)
			}
		})
	}
}

// TestGetNetCIDRSubnet_Functionality tests the getNetCIDRSubnet helper
func TestGetNetCIDRSubnet_Functionality(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		expected    string
		expectError bool
	}{
		{
			name:        "Double-slash format (layer3)",
			input:       "172.31.0.0/16/24",
			expected:    "172.31.0.0/16",
			expectError: false,
		},
		{
			name:        "Double-slash IPv6 format",
			input:       "2010:100:200::0/60/64",
			expected:    "2010:100:200::0/60",
			expectError: false,
		},
		{
			name:        "Single-slash format (layer2/localnet)",
			input:       "172.31.0.0/24",
			expected:    "172.31.0.0/24",
			expectError: false,
		},
		{
			name:        "Invalid format - no slash",
			input:       "172.31.0.0",
			expectError: true,
		},
		{
			name:        "Invalid format - too many slashes",
			input:       "172.31.0.0/16/24/32",
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := getNetCIDRSubnet(tt.input)
			if tt.expectError {
				if err == nil {
					t.Errorf("getNetCIDRSubnet(%q) expected error but got none", tt.input)
				}
			} else {
				if err != nil {
					t.Errorf("getNetCIDRSubnet(%q) unexpected error: %v", tt.input, err)
				}
				if result != tt.expected {
					t.Errorf("getNetCIDRSubnet(%q) = %q, expected %q", tt.input, result, tt.expected)
				}
			}
		})
	}
}
