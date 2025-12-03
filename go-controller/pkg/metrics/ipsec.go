package metrics

import (
	"fmt"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
)

type ipsecClient func(args ...string) (string, string, error)

func listEstablishedIPsecTunnels(ipsec ipsecClient) (sets.Set[string], error) {
	ipsecTunnels := sets.Set[string]{}
	stdout, stderr, err := ipsec("showstates")
	if err != nil {
		return ipsecTunnels, fmt.Errorf("failed to retrieve ipsec states, stderr: %v, err: %v",
			stderr, err)
	}
	if stdout == "" {
		return ipsecTunnels, fmt.Errorf("no IPsec tunnels found")
	}
	// sample output:
	// 000 #7: "ovn-60abc6-0-in-1":500 STATE_V2_ESTABLISHED_CHILD_SA (established Child SA); REKEY in 25883s; REPLACE in 26153s; IKE SA #5; idle;
	// 000 #7: "ovn-60abc6-0-in-1" esp.9d4cfa02@172.18.0.4 esp.950d136b@172.18.0.2 Traffic: ESPin=0B ESPout=0B ESPmax=2^63B
	// 000 #10: "ovn-60abc6-0-in-1":500 STATE_V2_ESTABLISHED_CHILD_SA (established Child SA); REKEY in 25527s; REPLACE in 26153s; newest; eroute owner; IKE SA #5; idle;
	// 000 #10: "ovn-60abc6-0-in-1" esp.fac8b437@172.18.0.4 esp.f4d763b9@172.18.0.2 Traffic: ESPin=0B ESPout=0B ESPmax=2^63B
	// 000 #5: "ovn-60abc6-0-out-1":500 STATE_V2_ESTABLISHED_IKE_SA (established IKE SA); REKEY in 25883s; REPLACE in 26153s; newest; idle;
	// 000 #6: "ovn-60abc6-0-out-1":500 STATE_V2_ESTABLISHED_CHILD_SA (established Child SA); REKEY in 25883s; REPLACE in 26153s; newest; eroute owner; IKE SA #5; idle;
	// 000 #6: "ovn-60abc6-0-out-1" esp.92964e3b@172.18.0.4 esp.4aa37686@172.18.0.2 Traffic: ESPin=0B ESPout=0B ESPmax=2^63B
	lines := strings.Split(stdout, "\n")
	re := regexp.MustCompile(`"([^"]*)"`) // compile once
	for _, line := range lines {
		if strings.Contains(line, "ESTABLISHED_CHILD_SA") {
			// Extract IPsec tunnel name which is enclosed with double quotes.
			matches := re.FindAllStringSubmatch(line, -1)
			for _, match := range matches {
				if len(match) != 2 {
					klog.Warningf("Invalid IPsec tunnel entry for ipsec showstates command: %s", line)
					continue
				}
				// match always contains two elements.
				// match[0]: tunnel name with quotes, e.g. "ovn-60abc6-0-in-1"
				// match[1]: tunnel name inside quotes, e.g. ovn-60abc6-0-in-1
				ipsecTunnels.Insert(match[1])
			}
		}
	}
	if ipsecTunnels.Len() == 0 {
		return ipsecTunnels, fmt.Errorf("established IPsec tunnels are not found")
	}
	return ipsecTunnels, nil
}
