package clusterinspection

import "os"

func IsIPv4Supported() bool {
	val, present := os.LookupEnv("KIND_IPV4_SUPPORT")
	return present && val == "true"
}

func IsIPv6Supported() bool {
	val, present := os.LookupEnv("KIND_IPV6_SUPPORT")
	return present && val == "true"
}
