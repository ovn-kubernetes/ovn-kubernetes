package external

import "os"

var (
	ContainerNetwork = "kind"
	ContainerIPv4    = ""
	ContainerIPv6    = ""
)

func init() {
	// When the env variable is specified, we use a different docker network for
	// containers acting as external gateways.
	// In a specific case where the variable is set to `host` we create only one
	// external container to act as an external gateway, as we can't create 2
	// because of overlapping ip/ports (like the bfd port).
	if exNetwork, found := os.LookupEnv("OVN_TEST_EX_GW_NETWORK"); found {
		ContainerNetwork = exNetwork
	}

	// When OVN_TEST_EX_GW_NETWORK is set to "host" we need to set the container's IP from outside
	if exHostIPv4, found := os.LookupEnv("OVN_TEST_EX_GW_IPV4"); found {
		ContainerIPv4 = exHostIPv4
	}

	if exHostIPv6, found := os.LookupEnv("OVN_TEST_EX_GW_IPV6"); found {
		ContainerIPv6 = exHostIPv6
	}
}
