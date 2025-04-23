package ip

import "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

func MatchIPv4StringFamily(ipStrings []string) (string, error) {
	return util.MatchIPStringFamily(false /*ipv4*/, ipStrings)
}

func MatchIPv6StringFamily(ipStrings []string) (string, error) {
	return util.MatchIPStringFamily(true /*ipv6*/, ipStrings)
}
