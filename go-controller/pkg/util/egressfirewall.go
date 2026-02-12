package util

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/miekg/dns"

	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	egressfirewallv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/egressfirewall/v1"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
)

const (
	// dnsRegex gives the regular expression for DNS names when DNSNameResolver is enabled.
	dnsRegex = `^(\*\.)?([a-zA-Z0-9]([-a-zA-Z0-9]{0,61}[a-zA-Z0-9])?\.)+[a-zA-Z0-9]([-a-zA-Z0-9]{0,61}[a-zA-Z0-9])?\.?$`
)

// ValidateAndGetEgressFirewallDNSDestination validates an egress firewall rule destination and returns
// the parsed contents of the destination.
func ValidateAndGetEgressFirewallDNSDestination(egressFirewallDestination egressfirewallv1.EgressFirewallDestination) (
	dnsName string,
	err error) {
	// Validate the egress firewall rule.
	if egressFirewallDestination.DNSName != "" {
		// Validate that DNS name is not wildcard when DNSNameResolver is not enabled.
		if !config.OVNKubernetesFeature.EnableDNSNameResolver && IsWildcard(egressFirewallDestination.DNSName) {
			return "", fmt.Errorf("wildcard dns name is not supported as rule destination, %s", egressFirewallDestination.DNSName)
		}
		// Validate that DNS name if DNSNameResolver is enabled.
		if config.OVNKubernetesFeature.EnableDNSNameResolver {
			exp := regexp.MustCompile(dnsRegex)
			if !exp.MatchString(egressFirewallDestination.DNSName) {
				return "", fmt.Errorf("invalid dns name used as rule destination, %s", egressFirewallDestination.DNSName)
			}
		}
		dnsName = egressFirewallDestination.DNSName
	}

	return
}

// IsWildcard checks if the domain name is wildcard.
func IsWildcard(dnsName string) bool {
	return strings.HasPrefix(dnsName, "*.")
}

// IsDNSNameResolverEnabled retuns true if both EgressFirewall
// and DNSNameResolver are enabled.
func IsDNSNameResolverEnabled() bool {
	return config.OVNKubernetesFeature.EnableEgressFirewall && config.OVNKubernetesFeature.EnableDNSNameResolver
}

// LowerCaseFQDN convert the DNS name to lower case fully qualified
// domain name.
func LowerCaseFQDN(dnsName string) string {
	return strings.ToLower(dns.Fqdn(dnsName))
}

// GetDNSNames iterates through the egress firewall rules and returns the DNS
// names present in them after validating the rules.
func GetDNSNames(ef *egressfirewallv1.EgressFirewall) []string {
	var dnsNameSlice []string
	for i, egressFirewallRule := range ef.Spec.Egress {
		if i > types.EgressFirewallStartPriority-types.MinimumReservedEgressFirewallPriority {
			klog.Warningf("egressFirewall for namespace %s has too many rules, the rest will be ignored", ef.Namespace)
			break
		}

		// Validate egress firewall rule destination and get the DNS name
		// if used in the rule.
		dnsName, err := ValidateAndGetEgressFirewallDNSDestination(egressFirewallRule.To)
		if err != nil {
			return []string{}
		}

		if dnsName != "" {
			dnsNameSlice = append(dnsNameSlice, LowerCaseFQDN(dnsName))
		}
	}

	return dnsNameSlice
}
