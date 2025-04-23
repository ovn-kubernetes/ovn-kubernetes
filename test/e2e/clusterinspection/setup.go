package clusterinspection

import "os"

func IsInterconnectEnabled() bool {
	val, present := os.LookupEnv("OVN_ENABLE_INTERCONNECT")
	return present && val == "true"
}
