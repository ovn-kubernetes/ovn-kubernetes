// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package allocators

import (
	"fmt"
	"math/big"
	"net"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/util/sets"
)

// subnetSpec partitions a large IP range into equal-size smaller subnets for
// allocation. It is constructed from a "base/outer/inner" spec string where
// base is the starting IP address, outer is the prefix length of the overall
// range and inner is the prefix length of each allocated subnet. For example,
// "10.0.0.0/8/20" represents 4096 /20 subnets within 10.0.0.0/8. Some of
// those subnets can be excluded to avoid collisions with infrastructure
// networks.
type subnetSpec struct {
	base     net.IP
	bits     int
	outer    int
	inner    int
	excluded sets.Set[int]
}

// newSubnetSpec creates a subnetSpec from a "base/outer/inner" spec string
// and a list of CIDRs to exclude from allocation. Exclusions can be any
// subnet: if one covers the entire outer range, it panics; if it has no
// overlap with the outer range, it is ignored; otherwise all inner candidates
// that overlap the exclusion are marked unavailable.
func newSubnetSpec(spec string, exclusions []string) subnetSpec {
	s, err := parseSubnetSpec(spec)
	if err != nil {
		panic(fmt.Sprintf("invalid subnet spec %q: %v", spec, err))
	}

	if len(exclusions) == 0 {
		return s
	}

	// compute exclusions
	outerNet := &net.IPNet{
		IP:   s.base,
		Mask: net.CIDRMask(s.outer, s.bits),
	}
	outerOnes, _ := outerNet.Mask.Size()
	baseInt := new(big.Int).SetBytes(s.base.To16())
	innerSize := new(big.Int).Lsh(big.NewInt(1), uint(s.bits-s.inner))
	s.excluded = sets.New[int]()
	for _, cidr := range exclusions {
		_, exclNet, err := net.ParseCIDR(cidr)
		if err != nil {
			panic(fmt.Sprintf("invalid exclusion %q for spec %q: %v", cidr, spec, err))
		}

		// exclusion is a super net, makes no sense
		exclOnes, _ := exclNet.Mask.Size()
		if exclNet.Contains(outerNet.IP) && exclOnes <= outerOnes {
			panic(fmt.Sprintf("exclusion %q covers the entire subnet spec range %q", cidr, spec))
		}

		// exclusion does not overlap, safely ignore
		if !overlaps(outerNet, exclNet) {
			continue
		}

		// Some math to compute the inner subnet indices excluded
		// get the first and last IP of the exclusion (as big int)
		exclStartInt := new(big.Int).SetBytes(exclNet.IP.To16())
		exclSize := new(big.Int).Lsh(big.NewInt(1), uint(s.bits-exclOnes))
		exclEndInt := new(big.Int).Add(exclStartInt, exclSize)
		exclEndInt.Sub(exclEndInt, big.NewInt(1))
		// find out how many inner sized subnets fit from base IP to first IP of
		// exclusion, that's the index of first inner subnet to exclude
		// i = (ip - base) / innerSize.
		startIdx := max(0, int(new(big.Int).Div(new(big.Int).Sub(exclStartInt, baseInt), innerSize).Int64()))
		// find out how many inner sized subnets fit from base IP to last IP of
		// exclusion, that's the index of the last inner subnet to exclude
		endIdx := min(s.total()-1, int(new(big.Int).Div(new(big.Int).Sub(exclEndInt, baseInt), innerSize).Int64()))
		for i := startIdx; i <= endIdx; i++ {
			s.excluded.Insert(i)
		}
	}

	return s
}

func overlaps(a, b *net.IPNet) bool {
	return a.Contains(b.IP) || b.Contains(a.IP)
}

func parseSubnetSpec(spec string) (subnetSpec, error) {
	lastSlash := strings.LastIndex(spec, "/")
	if lastSlash < 0 {
		return subnetSpec{}, fmt.Errorf("missing inner mask")
	}
	inner, err := strconv.Atoi(spec[lastSlash+1:])
	if err != nil {
		return subnetSpec{}, err
	}
	rest := spec[:lastSlash]
	secondSlash := strings.LastIndex(rest, "/")
	if secondSlash < 0 {
		return subnetSpec{}, fmt.Errorf("missing outer mask")
	}
	outer, err := strconv.Atoi(rest[secondSlash+1:])
	if err != nil {
		return subnetSpec{}, err
	}
	base := net.ParseIP(rest[:secondSlash])
	if base == nil {
		return subnetSpec{}, fmt.Errorf("parsing base addr %q", rest[:secondSlash])
	}
	bits := 32
	if base.To4() == nil {
		bits = 128
	}
	return subnetSpec{base: base, bits: bits, outer: outer, inner: inner}, nil
}

func (s subnetSpec) total() int {
	return 1 << (s.inner - s.outer)
}

func (s subnetSpec) usable() int {
	return s.total() - s.excluded.Len()
}

// nthFree returns the index of the n-th non-excluded subnet (1-indexed).
func (s subnetSpec) nthFree(n int) int {
	count := 0
	for i := range s.total() {
		if s.excluded.Has(i) {
			continue
		}
		count++
		if count == n {
			return i
		}
	}
	panic(fmt.Sprintf("nthFree: n=%d exceeds %d usable subnets in %s/%d/%d", n, s.usable(), s.base, s.outer, s.inner))
}

// cidr returns the CIDR of the idx-th /inner subnet within the /outer range.
func (s subnetSpec) cidr(idx int) string {
	if idx < 0 || idx >= s.total() {
		panic(fmt.Sprintf("cidr: idx=%d exceeds %d total subnets in %s/%d/%d", idx, s.total(), s.base, s.outer, s.inner))
	}
	baseInt := new(big.Int).SetBytes(s.base.To16())
	// each /inner subnet spans 2^(bits-inner) addresses; the idx-th starts
	// at base + idx * 2^(bits-inner)
	// e.g. for "10.0.0.0/8/20": idx=1408 → 10.0.0.0 + 1408*4096 → 10.88.0.0/20
	offset := new(big.Int).Lsh(big.NewInt(int64(idx)), uint(s.bits-s.inner))
	ip := make(net.IP, net.IPv6len)
	new(big.Int).Add(baseInt, offset).FillBytes(ip)
	return fmt.Sprintf("%s/%d", ip, s.inner)
}
