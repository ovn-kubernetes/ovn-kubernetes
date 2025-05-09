package metrics

import (
	"fmt"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/util/sets"
)

type ipsecClient func(args ...string) (string, string, error)

func listEstablishedIPsecTunnels(ipsec ipsecClient) (sets.Set[string], error) {
	ipsecTunnels := sets.Set[string]{}
	stdout, stderr, err := ipsec("showstates")
	if err != nil {
		return ipsecTunnels, fmt.Errorf("failed to retrieve ipsec states %v: %v", stderr, err)
	}
	if stdout == "" {
		return ipsecTunnels, nil
	}

	lines := strings.Split(string(stdout), "\n")
	for _, line := range lines {
		if strings.Contains(line, "ESTABLISHED_CHILD_SA") {
			matches := regexp.MustCompile(`"([^"]*)"`).FindAllStringSubmatch(line, -1)
			for _, m := range matches {
				ipsecTunnels.Insert(strings.Fields(m[1])...)
			}
		}
	}
	return ipsecTunnels, nil
}
