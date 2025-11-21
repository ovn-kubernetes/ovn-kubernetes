package util

import (
	"crypto/rand"
	"errors"
	"fmt"
	"hash/fnv"
	"net"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/urfave/cli/v2"
	"golang.org/x/exp/constraints"

	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	discoverylisters "k8s.io/client-go/listers/discovery/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	utilnet "k8s.io/utils/net"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
)

// OvnConflictBackoff is the backoff used for pod annotation update conflict
var OvnConflictBackoff = wait.Backoff{
	Steps:    2,
	Duration: 10 * time.Millisecond,
	Factor:   5.0,
	Jitter:   0.1,
}

var (
	rePciDeviceName = regexp.MustCompile(`^[0-9a-f]{4}:[0-9a-f]{2}:[01][0-9a-f]\.[0-7]$`)
	reAuxDeviceName = regexp.MustCompile(`^\w+.\w+.\d+$`)
)

// IsPCIDeviceName check if passed device id is a PCI device name
func IsPCIDeviceName(deviceID string) bool {
	return rePciDeviceName.MatchString(deviceID)
}

// IsAuxDeviceName check if passed device id is a Auxiliary device name
func IsAuxDeviceName(deviceID string) bool {
	return reAuxDeviceName.MatchString(deviceID)
}

// StringArg gets the named command-line argument or returns an error if it is empty
func StringArg(context *cli.Context, name string) (string, error) {
	val := context.String(name)
	if val == "" {
		return "", fmt.Errorf("argument --%s should be non-null", name)
	}
	return val, nil
}

// GetIPNetFullMask returns an IPNet object for IPV4 or IPV6 address with a full subnet mask
func GetIPNetFullMask(ipStr string) (*net.IPNet, error) {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return nil, fmt.Errorf("failed to parse IP %q", ipStr)
	}
	mask := GetIPFullMask(ip)
	return &net.IPNet{
		IP:   ip,
		Mask: mask,
	}, nil
}

// GetIPNetFullMaskFromIP returns an IPNet object for IPV4 or IPV6 address with a full subnet mask
func GetIPNetFullMaskFromIP(ip net.IP) *net.IPNet {
	mask := GetIPFullMask(ip)
	return &net.IPNet{
		IP:   ip,
		Mask: mask,
	}
}

// GetIPFullMaskString returns /32 if ip is IPV4 family and /128 if ip is IPV6 family
func GetIPFullMaskString(ip string) string {
	const (
		// IPv4FullMask is the maximum prefix mask for an IPv4 address
		IPv4FullMask = "/32"
		// IPv6FullMask is the maxiumum prefix mask for an IPv6 address
		IPv6FullMask = "/128"
	)

	if utilnet.IsIPv6(net.ParseIP(ip)) {
		return IPv6FullMask
	}
	return IPv4FullMask
}

// GetIPFullMask returns a full IPv4 IPMask if ip is IPV4 family or a full IPv6
// IPMask otherwise
func GetIPFullMask(ip net.IP) net.IPMask {
	if utilnet.IsIPv6(ip) {
		return net.CIDRMask(128, 128)
	}
	return net.CIDRMask(32, 32)
}

// GetK8sMgmtIntfName returns the management port name for a given node.
func GetK8sMgmtIntfName(nodeName string) string {
	return types.K8sPrefix + nodeName
}

// GetLegacyK8sMgmtIntfName returns legacy management ovs-port name
func GetLegacyK8sMgmtIntfName(nodeName string) string {
	if len(nodeName) > 11 {
		return types.K8sPrefix + (nodeName[:11])
	}
	return GetK8sMgmtIntfName(nodeName)
}

// GetNetworkScopedK8sMgmtHostIntfName returns the management port host interface name for a network id
// NOTE: network id is used instead of name so we don't reach the linux device name limit of 15 chars
func GetNetworkScopedK8sMgmtHostIntfName(networkID uint) string {
	intfName := types.K8sMgmtIntfNamePrefix + fmt.Sprintf("%d", networkID)
	// We are over linux 15 chars limit for network devices, let's trim it
	// for the prefix so we keep networkID as much as possible
	if len(intfName) > 15 {
		return intfName[:15]
	}
	return intfName
}

// GetWorkerFromGatewayRouter determines a node's corresponding worker switch name from a gateway router name
func GetWorkerFromGatewayRouter(gr string) string {
	return strings.TrimPrefix(gr, types.GWRouterPrefix)
}

// GetGatewayRouterFromNode determines a node's corresponding gateway router name
func GetGatewayRouterFromNode(node string) string {
	return types.GWRouterPrefix + node
}

// GetExtSwitchFromNode determines a node's corresponding gateway router name
func GetExtSwitchFromNode(node string) string {
	return types.ExternalSwitchPrefix + node
}

// GetExtPortName determines the name of a node's logical port to the external
// bridge.
func GetExtPortName(bridgeID, nodeName string) string {
	return bridgeID + "_" + nodeName
}

// GetPatchPortName determines the name of the patch port on the external
// bridge, which connects to br-int
func GetPatchPortName(bridgeID, nodeName string) string {
	return types.PatchPortPrefix + GetExtPortName(bridgeID, nodeName) + types.PatchPortSuffix
}

// GetNodeInternalAddrs returns the first IPv4 and/or IPv6 InternalIP defined
// for the node. On certain cloud providers (AWS) the egress IP will be added to
// the list of node IPs as an InternalIP address, we don't want to create the
// default allow logical router policies for that IP. Node IPs are ordered,
// meaning the egress IP will never be first in this list.
func GetNodeInternalAddrs(node *corev1.Node) (net.IP, net.IP) {
	var v4Addr, v6Addr net.IP
	for _, nodeAddr := range node.Status.Addresses {
		if nodeAddr.Type == corev1.NodeInternalIP {
			ip := utilnet.ParseIPSloppy(nodeAddr.Address)
			if !utilnet.IsIPv6(ip) && v4Addr == nil {
				v4Addr = ip
			} else if utilnet.IsIPv6(ip) && v6Addr == nil {
				v6Addr = ip
			}
		}
	}
	return v4Addr, v6Addr
}

// GetNodeAddresses returns all of the node's IPv4 and/or IPv6 annotated
// addresses as requested. Note that nodes not annotated will be ignored.
func GetNodeAddresses(ipv4, ipv6 bool, nodes ...*corev1.Node) (ipsv4 []net.IP, ipsv6 []net.IP, err error) {
	allCIDRs := sets.Set[string]{}
	for _, node := range nodes {
		ips, err := ParseNodeHostCIDRs(node)
		if IsAnnotationNotSetError(err) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		allCIDRs = allCIDRs.Insert(ips.UnsortedList()...)
	}

	for _, cidr := range allCIDRs.UnsortedList() {
		ip, _, err := net.ParseCIDR(cidr)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to get parse CIDR %v: %w", cidr, err)
		}
		if ipv4 && utilnet.IsIPv4(ip) {
			ipsv4 = append(ipsv4, ip)
		} else if ipv6 && utilnet.IsIPv6(ip) {
			ipsv6 = append(ipsv6, ip)
		}
	}
	return
}

// GetNodeChassisID returns the machine's OVN chassis ID
func GetNodeChassisID() (string, error) {
	chassisID, stderr, err := RunOVSVsctl("--if-exists", "get",
		"Open_vSwitch", ".", "external_ids:system-id")
	if err != nil {
		klog.Errorf("No system-id configured in the local host, "+
			"stderr: %q, error: %v", stderr, err)
		return "", err
	}
	if chassisID == "" {
		return "", fmt.Errorf("no system-id configured in the local host")
	}

	return chassisID, nil
}

// GetHybridOverlayPortName returns the name of the hybrid overlay switch port
// for a given node
func GetHybridOverlayPortName(nodeName string) string {
	return "int-" + nodeName
}

type annotationNotSetError struct {
	msg string
}

func (anse *annotationNotSetError) Error() string {
	return anse.msg
}

// newAnnotationNotSetError returns an error for an annotation that is not set
func newAnnotationNotSetError(format string, args ...interface{}) error {
	return &annotationNotSetError{msg: fmt.Sprintf(format, args...)}
}

// IsAnnotationNotSetError returns true if the error indicates that an annotation is not set
func IsAnnotationNotSetError(err error) bool {
	var annotationNotSetError *annotationNotSetError
	return errors.As(err, &annotationNotSetError)
}

type annotationAlreadySetError struct {
	msg string
}

func (aase *annotationAlreadySetError) Error() string {
	return aase.msg
}

// newAnnotationAlreadySetError returns an error for an annotation that is not set
func newAnnotationAlreadySetError(format string, args ...interface{}) error {
	return &annotationAlreadySetError{msg: fmt.Sprintf(format, args...)}
}

// IsAnnotationAlreadySetError returns true if the error indicates that an annotation is already set
func IsAnnotationAlreadySetError(err error) bool {
	var annotationAlreadySetError *annotationAlreadySetError
	return errors.As(err, &annotationAlreadySetError)
}

// HashforOVN hashes the provided input to make it a valid addressSet or portGroup name.
func HashForOVN(s string) string {
	h := fnv.New64a()
	_, err := h.Write([]byte(s))
	if err != nil {
		klog.Errorf("Failed to hash %s", s)
		return ""
	}
	hashString := strconv.FormatUint(h.Sum64(), 10)
	return fmt.Sprintf("a%s", hashString)
}

// FilterIPsSlice will filter a list of IPs by a list of CIDRs. By default,
// it will *remove* all IPs that match filter, unless keep is true.
//
// It is dual-stack aware.
func FilterIPsSlice(s []string, filter []net.IPNet, keep bool) []string {
	out := make([]string, 0, len(s))
ipLoop:
	for _, ipStr := range s {
		ip := net.ParseIP(ipStr)
		is4 := ip.To4() != nil

		for _, cidr := range filter {
			if is4 && cidr.IP.To4() != nil && cidr.Contains(ip) {
				if keep {
					out = append(out, ipStr)
					continue ipLoop
				} else {
					continue ipLoop
				}
			}
			if !is4 && cidr.IP.To4() == nil && cidr.Contains(ip) {
				if keep {
					out = append(out, ipStr)
					continue ipLoop
				} else {
					continue ipLoop
				}
			}
		}
		if !keep { // discard mode, and nothing matched.
			out = append(out, ipStr)
		}
	}

	return out
}

// IsClusterIP checks if the provided IP is a clusterIP
func IsClusterIP(svcVIP string) bool {
	ip := net.ParseIP(svcVIP)
	is4 := ip.To4() != nil
	for _, svcCIDR := range config.Kubernetes.ServiceCIDRs {
		if is4 && svcCIDR.IP.To4() != nil && svcCIDR.Contains(ip) {
			return true
		}
		if !is4 && svcCIDR.IP.To4() == nil && svcCIDR.Contains(ip) {
			return true
		}
	}
	return false
}

// InvalidPrimaryNetworkError indicates that the namespace requires a primary UDN, but no primary UDN exists yet
type InvalidPrimaryNetworkError struct {
	namespace string
}

func (m *InvalidPrimaryNetworkError) Error() string {
	return fmt.Sprintf("invalid primary network state for namespace %q: "+
		"a valid primary user defined network or network attachment definition custom resource, "+
		"and required namespace label %q must both be present",
		m.namespace, types.RequiredUDNNamespaceLabel)
}

func NewInvalidPrimaryNetworkError(namespace string) *InvalidPrimaryNetworkError {
	return &InvalidPrimaryNetworkError{namespace: namespace}
}

func IsInvalidPrimaryNetworkError(err error) bool {
	var invalidPrimaryNetworkError *InvalidPrimaryNetworkError
	return errors.As(err, &invalidPrimaryNetworkError)
}

func GetUserDefinedNetworkRole(isPrimary bool) string {
	networkRole := types.NetworkRoleSecondary
	if isPrimary {
		networkRole = types.NetworkRolePrimary
	}
	return networkRole
}

// GenerateExternalIDsForSwitchOrRouter returns the external IDs for logical switches and logical routers
// when it runs on a primary or secondary network. It returns an empty map
// when on the default cluster network, for backward compatibility.
func GenerateExternalIDsForSwitchOrRouter(netInfo NetInfo) map[string]string {
	externalIDs := make(map[string]string)
	if netInfo.IsUserDefinedNetwork() {
		externalIDs[types.NetworkExternalID] = netInfo.GetNetworkName()
		externalIDs[types.NetworkRoleExternalID] = GetUserDefinedNetworkRole(netInfo.IsPrimaryNetwork())
		externalIDs[types.TopologyExternalID] = netInfo.TopologyType()
	}
	return externalIDs
}

func GetUserDefinedNetworkLogicalPortName(podNamespace, podName, nadName string) string {
	return GetUserDefinedNetworkPrefix(nadName) + composePortName(podNamespace, podName)
}

func GetLogicalPortName(podNamespace, podName string) string {
	return composePortName(podNamespace, podName)
}

func GetNamespacePodFromCDNPortName(portName string) (string, string) {
	return decomposePortName(portName)
}

func GetUDNIfaceId(podNamespace, podName, nadName string) string {
	return GetUserDefinedNetworkPrefix(nadName) + composePortName(podNamespace, podName)
}

func GetIfaceId(podNamespace, podName string) string {
	return composePortName(podNamespace, podName)
}

// composePortName should be called both for LogicalPortName and iface-id
// because ovn-nb man says:
// Logical_Switch_Port.name must match external_ids:iface-id
// in the Open_vSwitch database’s Interface table,
// because hypervisors use external_ids:iface-id as a lookup key to
// identify the network interface of that entity.
func composePortName(podNamespace, podName string) string {
	return podNamespace + "_" + podName
}

func decomposePortName(s string) (string, string) {
	namespacePod := strings.Split(s, "_")
	if len(namespacePod) != 2 {
		return "", ""
	}
	return namespacePod[0], namespacePod[1]
}

func SliceHasStringItem(slice []string, item string) bool {
	for _, i := range slice {
		if i == item {
			return true
		}
	}
	return false
}

// StringSlice converts to a slice of the string representation of the input
// items
func StringSlice[T fmt.Stringer](items []T) []string {
	s := make([]string, len(items))
	for i := range items {
		s[i] = items[i].String()
	}
	return s
}

func SortedKeys[K constraints.Ordered, V any](m map[K]V) []K {
	keys := make([]K, len(m))
	i := 0
	for k := range m {
		keys[i] = k
		i++
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}

var chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ1234567890-"

// GenerateId returns a random id as a string with the requested length
func GenerateId(length int) string {
	charsLength := len(chars)
	b := make([]byte, length)
	_, err := rand.Read(b) // generates len(b) random bytes
	if err != nil {
		klog.Errorf("Failed when generating a random ID: %v", err)
		return ""
	}

	for i := 0; i < length; i++ {
		b[i] = chars[int(b[i])%charsLength]
	}
	return string(b)
}

// IsMirrorEndpointSlice checks if the provided EndpointSlice is meant for the user defined network
func IsMirrorEndpointSlice(endpointSlice *discoveryv1.EndpointSlice) bool {
	_, ok := endpointSlice.Labels[types.LabelUserDefinedServiceName]
	return ok
}

// IsDefaultEndpointSlice checks if the provided EndpointSlice is meant for the default network
func IsDefaultEndpointSlice(endpointSlice *discoveryv1.EndpointSlice) bool {
	_, ok := endpointSlice.Labels[discoveryv1.LabelServiceName]
	return ok
}

// IsEndpointSliceForNetwork checks if the provided EndpointSlice is meant for the given network
// if types.UserDefinedNetworkEndpointSliceAnnotation is set it compares it to the network name,
// otherwise it returns true if the network is the default
func IsEndpointSliceForNetwork(endpointSlice *discoveryv1.EndpointSlice, network NetInfo) bool {
	if endpointSliceNetwork, ok := endpointSlice.Annotations[types.UserDefinedNetworkEndpointSliceAnnotation]; ok {
		return endpointSliceNetwork == network.GetNetworkName()
	}
	return network.IsDefault()
}

func GetDefaultEndpointSlicesEventHandler(handlerFuncs cache.ResourceEventHandlerFuncs) cache.ResourceEventHandler {
	return GetEndpointSlicesEventHandlerForNetwork(handlerFuncs, &DefaultNetInfo{})
}

// GetEndpointSlicesEventHandlerForNetwork returns an event handler based on the provided handlerFuncs and netInfo.
// On the default network, it returns a handler that filters out the mirrored EndpointSlices. Conversely in
// a primary network it returns a handler that only keeps the mirrored EndpointSlices and filters out the original ones.
// Otherwise, returns handlerFuncs as is.
func GetEndpointSlicesEventHandlerForNetwork(handlerFuncs cache.ResourceEventHandlerFuncs, netInfo NetInfo) cache.ResourceEventHandler {
	var eventHandler cache.ResourceEventHandler
	eventHandler = handlerFuncs
	if !IsNetworkSegmentationSupportEnabled() {
		return eventHandler
	}

	var filterFunc func(obj interface{}) bool

	if netInfo.IsDefault() {
		// Filter out objects without the "kubernetes.io/service-name" label to exclude mirrored EndpointSlices
		filterFunc = func(obj interface{}) bool {
			if endpointSlice, ok := obj.(*discoveryv1.EndpointSlice); ok {
				return IsDefaultEndpointSlice(endpointSlice)
			}
			klog.Errorf("Failed to cast the object to *discovery.EndpointSlice: %v", obj)
			return true
		}

	} else if netInfo.IsPrimaryNetwork() {
		// Only consider mirrored endpointslices for the given network
		filterFunc = func(obj interface{}) bool {
			if endpointSlice, ok := obj.(*discoveryv1.EndpointSlice); ok {
				isDefault := IsDefaultEndpointSlice(endpointSlice)
				isForThisNetwork := IsEndpointSliceForNetwork(endpointSlice, netInfo)
				return !isDefault && isForThisNetwork
			}
			klog.Errorf("Failed to cast the object to *discovery.EndpointSlice: %v", obj)
			return true
		}
	}
	if filterFunc != nil {
		eventHandler = cache.FilteringResourceEventHandler{
			FilterFunc: filterFunc,
			Handler:    handlerFuncs,
		}
	}

	return eventHandler
}

// GetEndpointSlicesBySelector returns a list of EndpointSlices in a given namespace by the label selector
func GetEndpointSlicesBySelector(namespace string, labelSelector metav1.LabelSelector, endpointSliceLister discoverylisters.EndpointSliceLister) ([]*discoveryv1.EndpointSlice, error) {
	selector, err := metav1.LabelSelectorAsSelector(&labelSelector)
	if err != nil {
		return nil, err
	}
	return endpointSliceLister.EndpointSlices(namespace).List(selector)
}

// GetServiceEndpointSlices returns the endpointSlices associated with a service for the specified network
// if network is DefaultNetworkName the default endpointSlices are returned, otherwise the function looks for mirror endpointslices
// for the specified network.
func GetServiceEndpointSlices(namespace, svcName, network string, endpointSliceLister discoverylisters.EndpointSliceLister) ([]*discoveryv1.EndpointSlice, error) {
	var selector metav1.LabelSelector
	if network == types.DefaultNetworkName {
		selector = metav1.LabelSelector{MatchLabels: map[string]string{
			discoveryv1.LabelServiceName: svcName,
		}}
		return GetEndpointSlicesBySelector(namespace, selector, endpointSliceLister)
	}

	selector = metav1.LabelSelector{MatchLabels: map[string]string{
		types.LabelUserDefinedServiceName: svcName,
	}}
	endpointSlices, err := GetEndpointSlicesBySelector(namespace, selector, endpointSliceLister)
	if err != nil {
		return nil, fmt.Errorf("failed to list endpoint slices for service %s/%s: %w", namespace, svcName, err)
	}
	networkEndpointSlices := make([]*discoveryv1.EndpointSlice, 0, len(endpointSlices))
	for _, endpointSlice := range endpointSlices {
		if endpointSlice.Annotations[types.UserDefinedNetworkEndpointSliceAnnotation] == network {
			networkEndpointSlices = append(networkEndpointSlices, endpointSlice)
		}
	}

	return networkEndpointSlices, nil
}

// IsUDNEnabledService checks whether the provided namespaced name key is a UDN enabled service specified in config.Default.UDNAllowedDefaultServices
func IsUDNEnabledService(key string) bool {
	for _, enabledService := range config.Default.UDNAllowedDefaultServices {
		if enabledService == key {
			return true
		}
	}
	return false
}

// ServiceFromEndpointSlice returns the namespaced name of the service that corresponds to the given endpointSlice
// in the given network. If the service label is missing the returned namespaced name and the error are nil.
func ServiceFromEndpointSlice(eps *discoveryv1.EndpointSlice, netName string) (*k8stypes.NamespacedName, error) {
	labelKey := discoveryv1.LabelServiceName
	if netName != types.DefaultNetworkName {
		if eps.Annotations[types.UserDefinedNetworkEndpointSliceAnnotation] != netName {
			return nil, fmt.Errorf("endpointslice %s/%s does not belong to %s network", eps.Namespace, eps.Name, netName)
		}
		labelKey = types.LabelUserDefinedServiceName
	}
	svcName, found := eps.Labels[labelKey]
	if !found {
		return nil, nil
	}

	if svcName == "" {
		return nil, fmt.Errorf("endpointslice %s/%s has empty svcName for label %s in network %s",
			eps.Namespace, eps.Name, labelKey, netName)
	}

	return &k8stypes.NamespacedName{Namespace: eps.Namespace, Name: svcName}, nil
}

// GetMirroredEndpointSlices retrieves all EndpointSlices in the given namespace that are managed
// by the controller and are mirrored from the sourceName EndpointSlice.
func GetMirroredEndpointSlices(controller, sourceName, namespace string, endpointSliceLister discoverylisters.EndpointSliceLister) (ret []*discoveryv1.EndpointSlice, err error) {
	mirrorEndpointSliceSelector := labels.Set(map[string]string{
		discoveryv1.LabelManagedBy: controller,
	}).AsSelectorPreValidated()
	allMirroredEndpointSlices, err := endpointSliceLister.EndpointSlices(namespace).List(mirrorEndpointSliceSelector)
	if err != nil {
		return nil, err
	}

	var mirroredEndpointSlices []*discoveryv1.EndpointSlice
	for _, endpointSlice := range allMirroredEndpointSlices {
		if val, exists := endpointSlice.Annotations[types.SourceEndpointSliceAnnotation]; exists && val == sourceName {
			mirroredEndpointSlices = append(mirroredEndpointSlices, endpointSlice)
		}
	}
	return mirroredEndpointSlices, nil
}

func MustParseCIDR(cidr string) *net.IPNet {
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		panic(fmt.Sprintf("failed to parse CIDR %q: %v", cidr, err))
	}
	return ipNet
}

// GetServicePortKey creates a unique identifier key for a service port using protocol and name.
// e.g. GetServicePortKey("TCP", "http") returns "TCP/http".
func GetServicePortKey(protocol corev1.Protocol, name string) string {
	return fmt.Sprintf("%s/%s", protocol, name)
}

// IPPort represents an IP address and port combination for load balancer destinations.
// e.g. IPPort{IP: "192.168.1.10", Port: 8080}.
type IPPort struct {
	IP   string
	Port int32
}

// LBEndpointsList is a slice of LBEndpoints, typically used when a service port maps to multiple backend ports.
// e.g. []LBEndpoints{{Port: 8080, V4IPs: []string{"192.168.1.10"}}, {Port: 9090, V4IPs: []string{"192.168.1.11"}}}.
type LBEndpointsList []LBEndpoints

// GetV4Destinations returns all IPv4 destinations from all LBEndpoints in the list.
func (lel LBEndpointsList) GetV4Destinations() []IPPort {
	destinations := []IPPort{}
	for _, le := range lel {
		destinations = append(destinations, le.GetV4Destinations()...)
	}
	return destinations
}

// GetV6Destinations returns all IPv6 destinations from all LBEndpoints in the list.
func (lel LBEndpointsList) GetV6Destinations() []IPPort {
	destinations := []IPPort{}
	for _, le := range lel {
		destinations = append(destinations, le.GetV6Destinations()...)
	}
	return destinations
}

// Equals returns true if the 2 provided LBEndpointsLists are equal. Equality
// is not determined by order of elements.
// IP addresses will be converted into net.IP representation before being compared. This means that invalid IP addresses
// will evaluate to nil and be compared as such.
func (lel LBEndpointsList) Equals(other LBEndpointsList) bool {
	if len(lel) != len(other) {
		return false
	}
	foundIndexes := make([]bool, len(other))
	for _, lbe := range lel {
		found := false
		for i, lbeOther := range other {
			if lbe.Equals(lbeOther) && !foundIndexes[i] {
				found = true
				foundIndexes[i] = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// Insert adds the provided element in the LBEndpointsList, ordered by port number.
func (lel LBEndpointsList) Insert(toInsert LBEndpoints) LBEndpointsList {
	i := 0
	for ; i < len(lel); i++ {
		if lel[i].Port == toInsert.Port {
			lel[i] = lel[i].Merge(toInsert)
			return lel
		}
		if lel[i].Port > toInsert.Port {
			// Use full slice expression [:i:i] to avoid modifying the original slice.
			result := append(lel[:i:i], toInsert)
			return append(result, lel[i:]...)
		}
	}
	return append(lel, toInsert)
}

// LBEndpoints contains load balancer endpoint information with IPv4 and IPv6 addresses.
// Port is the endpoint port (the one exposed by the pod) and IPs are the IP addresses of the backend pods.
// e.g. LBEndpoints{Port: 8080, V4IPs: []string{"192.168.1.10", "192.168.1.11"}, V6IPs: []string{"2001:db8::1"}}.
// TBD: currently, OVNK only supports a single backend port per named port.
type LBEndpoints struct {
	Port  int32
	V4IPs []string
	V6IPs []string
}

// GetV4Destinations builds IPv4 destination mappings from endpoint addresses to ports.
// e.g. for V4IPs ["192.168.1.10", "192.168.1.11"] and Port 8080, returns
// []IPPort{{IP: "192.168.1.10", Port: 8080}, {IP: "192.168.1.11", Port: 8080}}.
func (le LBEndpoints) GetV4Destinations() []IPPort {
	destinations := []IPPort{}
	for _, ip := range le.V4IPs {
		destinations = append(destinations, IPPort{IP: ip, Port: le.Port})
	}
	return destinations
}

// GetV6Destinations builds IPv6 destination mappings from endpoint addresses to ports.
// e.g. for V6IPs ["2001:db8::1", "2001:db8::2"] and Port 8080, returns
// []IPPort{{IP: "2001:db8::1", Port: 8080}, {IP: "2001:db8::2", Port: 8080}}.
func (le LBEndpoints) GetV6Destinations() []IPPort {
	destinations := []IPPort{}
	for _, ip := range le.V6IPs {
		destinations = append(destinations, IPPort{IP: ip, Port: le.Port})
	}
	return destinations
}

// Empty returns true if the LBEndpoints contains neither IPv4 nor IPv6 addresses.
func (le LBEndpoints) Empty() bool {
	return len(le.V4IPs) == 0 && len(le.V6IPs) == 0
}

// Equals compares 2 LBEndpoints for equality, ignoring order of IP addresses.
// IP addresses will be converted into net.IP representation before being compared. This means that invalid IP addresses
// will evaluate to nil and be compared as such.
func (le LBEndpoints) Equals(other LBEndpoints) bool {
	if le.Port != other.Port {
		return false
	}
	if len(le.V4IPs) != len(other.V4IPs) {
		return false
	}
	if len(le.V6IPs) != len(other.V6IPs) {
		return false
	}

	foundIndexes := make([]bool, len(other.V4IPs))
	for _, ip := range le.V4IPs {
		found := false
		ipParsed := net.ParseIP(ip)
		for i, ipOther := range other.V4IPs {
			if !foundIndexes[i] && ipParsed.Equal(net.ParseIP(ipOther)) {
				found = true
				foundIndexes[i] = true
				break
			}
		}
		if !found {
			return false
		}
	}

	foundIndexes = make([]bool, len(other.V6IPs))
	for _, ip := range le.V6IPs {
		found := false
		ipParsed := net.ParseIP(ip)
		for i, ipOther := range other.V6IPs {
			if !foundIndexes[i] && ipParsed.Equal(net.ParseIP(ipOther)) {
				found = true
				foundIndexes[i] = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// ExtractHostNetworkEndpoints takes the LBEndpoints and divides it into 2: the first returned value,
// hostNetworked, contains only addresses that match the provided nodeAddresses. The second returned value,
// notHostNetworked, contains all other addresses.
func (le LBEndpoints) ExtractHostNetworkEndpoints(nodeAddresses []net.IP) (hostNetworked, notHostNetworked LBEndpoints) {
	hostNetworked.Port = le.Port
	notHostNetworked.Port = le.Port
	for _, v4ip := range le.V4IPs {
		isHostNetworked := false
		for _, nodeAddress := range nodeAddresses {
			if !utilnet.IsIPv6(nodeAddress) && nodeAddress.Equal(net.ParseIP(v4ip)) {
				isHostNetworked = true
				hostNetworked.V4IPs = append(hostNetworked.V4IPs, v4ip)
				break
			}
		}
		if !isHostNetworked {
			notHostNetworked.V4IPs = append(notHostNetworked.V4IPs, v4ip)
		}
	}
	for _, v6ip := range le.V6IPs {
		isHostNetworked := false
		for _, nodeAddress := range nodeAddresses {
			if utilnet.IsIPv6(nodeAddress) && nodeAddress.Equal(net.ParseIP(v6ip)) {
				isHostNetworked = true
				hostNetworked.V6IPs = append(hostNetworked.V6IPs, v6ip)
				break
			}
		}
		if !isHostNetworked {
			notHostNetworked.V6IPs = append(notHostNetworked.V6IPs, v6ip)
		}
	}
	return
}

// Merge returns the result of the merge between the current endpoint with the provided endpoint if Port matches,
// otherwise it will return the current endpoint. IP addresses are string sorted so the result is guaranteed to be
// stable, with duplicate IP addresses being compacted into a single occurrence.
func (le LBEndpoints) Merge(other LBEndpoints) LBEndpoints {
	if le.Port != other.Port {
		return le
	}
	merged := LBEndpoints{
		Port:  le.Port,
		V4IPs: append(le.V4IPs, other.V4IPs...),
		V6IPs: append(le.V6IPs, other.V6IPs...),
	}
	slices.Sort(merged.V4IPs)
	slices.Sort(merged.V6IPs)
	merged.V4IPs = slices.Compact(merged.V4IPs)
	merged.V6IPs = slices.Compact(merged.V6IPs)
	return merged
}

// PortToLBEndpointsList maps service port keys (protocol + service port name) to load balancer endpoints lists.
// e.g. map["TCP/http"] = LBEndpointsList{LBEndpoints{Port: 8080, V4IPs: []string{"192.168.1.10"}}}.
type PortToLBEndpointsList map[string]LBEndpointsList

// Equals compares two PortToLBEndpointsList for equality. Equality is not determined by element order.
// IP addresses will be converted into net.IP representation before being compared. This means that invalid IP addresses
// will evaluate to nil and be compared as such.
func (p PortToLBEndpointsList) Equals(other PortToLBEndpointsList) bool {
	if len(p) != len(other) {
		return false
	}
	for i := range p {
		peElem, ok := other[i]
		if !ok {
			return false
		}
		if !p[i].Equals(peElem) {
			return false
		}
	}
	return true
}

// Empty returns true if the PortToLBEndpointsList has no entries.
func (p PortToLBEndpointsList) Empty() bool {
	return len(p) == 0
}

// GetLBEndpoints returns the LBEndpoints belonging to key, or an empty LBEndpointsList + an error otherwise.
func (p PortToLBEndpointsList) GetLBEndpoints(key string) (LBEndpointsList, error) {
	if lbe, ok := p[key]; ok {
		return lbe, nil
	}
	return LBEndpointsList{}, fmt.Errorf("cannot find key %q in PortToLBEndpoints %+v", key, p)
}

// ExtractHostNetworkEndpoints takes the PortToLBEndpointsList and divides it into 2: the first returned value,
// hostNetworked, contains only addresses that match the provided nodeAddresses. The second returned value,
// notHostNetworked, contains all other addresses.
func (p PortToLBEndpointsList) ExtractHostNetworkEndpoints(nodeAddresses []net.IP) (
	hostNetworked, notHostNetworked PortToLBEndpointsList) {
	hostNetworked = make(PortToLBEndpointsList)
	notHostNetworked = make(PortToLBEndpointsList)
	for port, lbel := range p {
		for _, lbe := range lbel {
			lbeHostNet, lbeNotHostNet := lbe.ExtractHostNetworkEndpoints(nodeAddresses)
			if !lbeHostNet.Empty() {
				hostNetworked[port] = append(hostNetworked[port], lbeHostNet)
			}
			if !lbeNotHostNet.Empty() {
				notHostNetworked[port] = append(notHostNetworked[port], lbeNotHostNet)
			}
		}
	}
	return
}

// PortToNodeToLBEndpointsList maps service port keys to node names and their load balancer endpoints.
// e.g. map["TCP/http"]["node1"] = LBEndpointsList{LBEndpoints{{Port: 8080, V4IPs: []string{"192.168.1.10"}}}}.
type PortToNodeToLBEndpointsList map[string]map[string]LBEndpointsList

// GetNode extracts all port endpoints for a specific node from the PortToNodeToLBEndpointsList.
// e.g. for PortToNodeToLBEndpoints{"TCP/http": {"node1": []LBEndpoints{{Port: 8080, V4IPs: ["192.168.1.10"]}}, "node2": []LBEndpoints{{Port: 8080, V4IPs: ["192.168.1.11"]}}}}
// and node "node1", returns PortToLBEndpoints{"TCP/http": []LBEndpoints{{Port: 8080, V4IPs: ["192.168.1.10"]}}}.
func (p PortToNodeToLBEndpointsList) GetNode(node string) PortToLBEndpointsList {
	r := make(PortToLBEndpointsList)
	for port, nodeToLBEndpoints := range p {
		if lbe, ok := nodeToLBEndpoints[node]; ok {
			r[port] = lbe
		}
	}
	return r
}

// Get returns the NodeToLbEndpointsList that belongs to the provided Port. Can never return a nil value, instead
// it will provide an initialized NodeToLbEndpointsList if the port string cannot be found.
func (p PortToNodeToLBEndpointsList) Get(port string) NodeToLBEndpointsList {
	if v, ok := p[port]; ok && v != nil {
		return v
	}
	return make(NodeToLBEndpointsList)
}

// GetEndpointsForService extracts endpoints from EndpointSlices for a given Service.
// It returns two maps.
// 1. Global endpoints: maps service port keys ("protocol/portname") to all endpoint addresses (if needsGlobalEndpoints).
// 2. Local endpoints: maps service port keys to per-node endpoint addresses (if needsLocalEndpoints).
//
// This method is common logic for both nodePortWatcher and the services Controller to build their
// service endpoints
//
// Special handling when service is nil:
// When service is nil (typically during deletion scenarios), all endpoint ports are accepted
// and processed without validation against service port specifications. This allows cleanup
// operations to proceed even when the service object is no longer available (needed for
// the nodePortWatcher).
//
// When service is not nil:
// Endpoint ports are validated against the service port specifications, and only matching
// ports are processed to ensure consistency with the service configuration.
//
// Parameters:
//   - slices: EndpointSlices associated with the service
//   - service: The Kubernetes Service object (nil during deletion scenarios)
//   - nodes: Set of node names in the OVN zone (used to filter local endpoints)
//   - needsGlobalEndpoints: request to populate PortToLBEndpoints
//   - needsLocalEndpoints: request to populate PortToNodeToLBEndpoints
//
// Returns:
//   - PortToLBEndpointsList: Global endpoint list mapping by port (empty if not needed)
//   - PortToNodeToLBEndpointsList: Per-node endpoint list mapping by port (empty if not needed)
//   - error: Validation errors encountered during processing
//
// Example output:
//
//	Global: {"TCP/http": {{Port: 8080, V4IPs: ["192.168.1.10", "192.168.1.11"]}}}
//	Local:  {"TCP/http": {"node1": {{Port: 8080, V4IPs: ["192.168.1.10"]}}}}
//
// Validation requirements:
//   - EndpointSlice port names must match Service port names (when service is not nil)
//   - Only one protocol per port name is supported (Kubernetes requirement).
func GetEndpointsForService(endpointSlices []*discoveryv1.EndpointSlice, service *corev1.Service,
	nodes sets.Set[string], needsGlobalEndpoints, needsLocalEndpoints bool) (PortToLBEndpointsList, PortToNodeToLBEndpointsList, error) {

	var validationErrors []error
	globalEndpoints := PortToLBEndpointsList{}
	localEndpoints := PortToNodeToLBEndpointsList{}

	addValidationError := func(msg string, detail interface{}) {
		ns, name := "<unknown>", "<unknown>"
		if service != nil {
			ns, name = service.GetNamespace(), service.GetName()
		}
		validationErrors = append(validationErrors, fmt.Errorf("%s for service \"%s/%s\": %v", msg, ns, name, detail))
	}

	// Parse endpoint slices into structured format: portName -> protocol -> portNumber -> endpoints.
	targetEndpoints := newTargetEndpoints(endpointSlices)

	// Build list of valid service port keys, if a non-nil service was provided. Otherwise, if service is nil, this
	// is a deletion (at least for the iptables / nftables logic) and thus accept all endpoints.
	validServicePortKeys := map[string]bool{}
	if service != nil {
		for _, servicePort := range service.Spec.Ports {
			name := GetServicePortKey(servicePort.Protocol, servicePort.Name)
			validServicePortKeys[name] = true
		}
	}

	for portName, protocolMap := range targetEndpoints {
		for protocol, portNumberMap := range protocolMap {
			// If service is not nil, there's a valid 1 to 1 mapping of service name + protocol to slice name + protocol.
			// Therefore, go through all ports of the service, and skip if no match found.
			slicePortKey := GetServicePortKey(protocol, portName)
			if service != nil && !validServicePortKeys[slicePortKey] {
				continue
			}

			// Due to the way how newTargetEndpoints is implemented, portNumberMap can never be empty.
			// However, be defensive here and verify nevertheless.
			if len(portNumberMap) == 0 {
				addValidationError("service protocol has no associated endpoints",
					fmt.Sprintf("servicePortKey %q", slicePortKey))
				continue
			}

			// Build global endpoint mapping.
			if needsGlobalEndpoints {
				lbe, err := buildLBEndpointsList(service, portNumberMap)
				if err != nil {
					klog.Warningf("Failed to build global endpoints for port %s: %v", slicePortKey, err)
				} else if len(lbe) > 0 {
					globalEndpoints[slicePortKey] = lbe
				}
			}
			// Build per-node endpoint mapping if needed for traffic policies.
			if needsLocalEndpoints {
				if nlbe, err := buildNodeLBEndpointsList(service, portNumberMap, nodes); err != nil {
					klog.Warningf("Failed to build local endpoints for port %s: %v", slicePortKey, err)
				} else if len(nlbe) > 0 {
					localEndpoints[slicePortKey] = nlbe
				}
			}
		}
	}

	// Log endpoint mappings for debugging.
	serviceString := ""
	if service != nil {
		serviceString = fmt.Sprintf(" for %s/%s", service.Namespace, service.Name)
	}
	if needsGlobalEndpoints {
		klog.V(5).Infof("Global endpoints%s: %v", serviceString, globalEndpoints)
	}
	if needsLocalEndpoints {
		klog.V(5).Infof("Local endpoints%s: %v", serviceString, localEndpoints)
	}

	return globalEndpoints, localEndpoints, errors.Join(validationErrors...)
}

// FindServicePortForEndpointSlicePort returns the ServicePort that corresponds to an EndpointSlice port
// by matching the port name and protocol. This is the canonical way to map EndpointSlice ports to
// Service ports, as Kubernetes guarantees that ServicePort.Name matches EndpointPort.Name.
func FindServicePortForEndpointSlicePort(service *corev1.Service, endpointslicePortName string, endpointslicePortProtocol corev1.Protocol) (*corev1.ServicePort, error) {
	if service == nil {
		return nil, fmt.Errorf("unable to resolve port for endpointslice %q/%q: service is nil",
			endpointslicePortName, endpointslicePortProtocol)
	}
	for _, servicePort := range service.Spec.Ports {
		if servicePort.Name == endpointslicePortName && servicePort.Protocol == endpointslicePortProtocol {
			return &servicePort, nil
		}
	}
	return nil, fmt.Errorf("service %s/%s has no port with name %q and protocol %s",
		service.Namespace, service.Name, endpointslicePortName, endpointslicePortProtocol)
}

// groupEndpointsByNode organizes a list of endpoints by their associated node names.
// Endpoints without a NodeName are skipped, as they cannot be assigned to specific nodes.
// This is used for building per-node endpoint mappings for local traffic policies.
//
// Parameters:
//   - endpoints: List of discovery endpoints to group
//
// Returns:
//   - map[string][]discoveryv1.Endpoint: Node name to endpoints mapping
func groupEndpointsByNode(endpoints []discoveryv1.Endpoint) map[string][]discoveryv1.Endpoint {
	nodeEndpoints := map[string][]discoveryv1.Endpoint{}
	for _, endpoint := range endpoints {
		if endpoint.NodeName == nil {
			continue
		}
		nodeName := *endpoint.NodeName
		nodeEndpoints[nodeName] = append(nodeEndpoints[nodeName], endpoint)
	}
	return nodeEndpoints
}

// nodeToLBEndpoints maps node names to their load balancer endpoints.
// e.g. map["node1"] = LBEndpoints{Port: 8080, V4IPs: []string{"192.168.1.10"}}.
// This struct is a helper that's only used by buildNodeLBEndpoints.
type nodeToLBEndpoints map[string]LBEndpoints

// NodeToLBEndpointsList maps node names to lists of load balancer endpoints.
// This type supports services with named ports that resolve to different numeric ports across pods.
// e.g. map["node1"] = []LBEndpoints{{Port: 8080, V4IPs: []string{"192.168.1.10"}}, {Port: 9090, V4IPs: []string{"192.168.1.11"}}}.
type NodeToLBEndpointsList map[string]LBEndpointsList

// buildNodeLBEndpointsList creates a per-node mapping of load balancer endpoint lists.
// It processes multiple port numbers and their associated endpoints, organizing them by node.
// Only nodes present in the provided node set are included in the result.
// This function supports services with named ports that resolve to different numeric ports across pods.
//
// Parameters:
//   - service: The Kubernetes Service object (for endpoint filtering)
//   - portNumberMap: Map of port numbers to their endpoint lists
//   - nodes: Set of valid node names to include
//
// Returns:
//   - NodeToLBEndpointsList: Map of node names to their LBEndpointsList
//   - error: Any errors encountered while building the endpoints
func buildNodeLBEndpointsList(service *corev1.Service, portNumberMap map[int32][]discoveryv1.Endpoint, nodes sets.Set[string]) (NodeToLBEndpointsList, error) {
	nlbes := make(NodeToLBEndpointsList)
	var errs []error

	for port, endpoints := range portNumberMap {
		nlbe, err := buildNodeLBEndpoints(service, port, endpoints, nodes)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for node, lbe := range nlbe {
			if _, ok := nlbes[node]; !ok {
				nlbes[node] = LBEndpointsList{}
			}
			nlbes[node] = nlbes[node].Insert(lbe)
		}
	}
	return nlbes, errors.Join(errs...)
}

// buildNodeLBEndpoints creates a per-node mapping of load balancer endpoints.
// Only nodes present in the provided node set are included in the result.
// This is used for services with local traffic policies that require per-node endpoint tracking.
// This function is a helper which is used by buildNodeLBEndpointsList only.
//
// Parameters:
//   - service: The Kubernetes Service object (for endpoint filtering)
//   - portNumber: The target port number for the endpoints
//   - endpoints: List of endpoints to process
//   - nodes: Set of valid node names to include
//
// Returns:
//   - map[string]LBEndpoints: Node name to LBEndpoints mapping
func buildNodeLBEndpoints(service *corev1.Service, portNumber int32, endpoints []discoveryv1.Endpoint, nodes sets.Set[string]) (nodeToLBEndpoints, error) {
	nodeLBEndpoints := nodeToLBEndpoints{}

	nodeEndpoints := groupEndpointsByNode(endpoints)
	for node, nodeEndpoints := range nodeEndpoints {
		if !nodes.Has(node) {
			continue
		}
		lbe, err := buildLBEndpoints(service, portNumber, nodeEndpoints)
		if err != nil {
			klog.Warningf("Failed to build node endpoints for node %s port %d: %v", node, portNumber, err)
			continue
		}
		nodeLBEndpoints[node] = lbe
	}

	if len(nodeLBEndpoints) == 0 {
		return nodeLBEndpoints, fmt.Errorf("empty node lb endpoints")
	}
	return nodeLBEndpoints, nil
}

// buildLBEndpointsList builds an LBEndpointsList list from the provided service and portNumberMap. The resulting
// LBEndpoints objects are built with buildLBEndpoints.
func buildLBEndpointsList(service *corev1.Service, portNumberMap map[int32][]discoveryv1.Endpoint) (LBEndpointsList, error) {
	var lbes LBEndpointsList
	var errs []error
	for port, endpoints := range portNumberMap {
		lbe, err := buildLBEndpoints(service, port, endpoints)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		lbes = lbes.Insert(lbe)
	}
	return lbes, errors.Join(errs...)
}

// buildLBEndpoints constructs an LBEndpoints structure from a list of discovery endpoints.
// It filters endpoints for eligibility, separates IPv4 and IPv6 addresses, and returns
// an empty LBEndpoints if no valid addresses are found.
//
// Parameters:
//   - service: The Kubernetes Service object (used for endpoint eligibility filtering)
//     service may be nil!
//   - port: The target port number for the endpoints
//   - endpoints: List of discovery endpoints to process
//
// Returns:
//   - LBEndpoints: Structure containing IPv4/IPv6 addresses and port number
func buildLBEndpoints(service *corev1.Service, port int32, endpoints []discoveryv1.Endpoint) (LBEndpoints, error) {
	addresses := GetEligibleEndpointAddresses(endpoints, service)
	v4IPs, v4ErrorNoIP := MatchAllIPStringFamily(false, addresses)
	v6IPs, v6ErrorNoIP := MatchAllIPStringFamily(true, addresses)

	if v4ErrorNoIP != nil && v6ErrorNoIP != nil {
		if service != nil {
			return LBEndpoints{}, fmt.Errorf("empty IP address endpoints for service %s/%s", service.Namespace, service.Name)
		} else {
			return LBEndpoints{}, fmt.Errorf("empty IP address endpoints")
		}
	}

	if port <= 0 || port > 65535 {
		if service != nil {
			return LBEndpoints{}, fmt.Errorf("invalid endpoint port %d for service %s/%s: port must be between 1-65535",
				port, service.Namespace, service.Name)
		} else {
			return LBEndpoints{}, fmt.Errorf("invalid endpoint port %d: port must be between 1-65535", port)
		}
	}

	return LBEndpoints{
		V4IPs: v4IPs,
		V6IPs: v6IPs,
		Port:  port,
	}, nil
}

// targetEndpoints provides a hierarchical mapping of endpoint data from EndpointSlices.
// Structure: port name -> protocol -> port number -> list of endpoints
// Example:
//
//	targetEndpoints{
//	  "http": {
//	    corev1.ProtocolTCP: {
//	      8080: []discoveryv1.Endpoint{...}
//	    }
//	  }
//	}
type targetEndpoints map[string]map[corev1.Protocol]map[int32][]discoveryv1.Endpoint

// addEndpoint adds a discovery endpoint to the TargetEndpoints structure.
// It initializes nested maps as needed to maintain the hierarchical structure.
// Multiple endpoints can be added for the same port name/protocol/port number combination.
// Note: This method does _not_ verify that only a single protocol is added.
//
// Parameters:
//   - portName: Name of the port (can be empty string for unnamed ports)
//   - proto: Protocol (TCP, UDP, SCTP)
//   - portNumber: Target port number
//   - endpoint: The discovery endpoint to add
func (te targetEndpoints) addEndpoint(portName string, proto corev1.Protocol, portNumber int32, endpoint discoveryv1.Endpoint) {
	if _, ok := te[portName]; !ok {
		te[portName] = make(map[corev1.Protocol]map[int32][]discoveryv1.Endpoint)
	}
	if _, ok := te[portName][proto]; !ok {
		te[portName][proto] = make(map[int32][]discoveryv1.Endpoint)
	}
	if _, ok := te[portName][proto][portNumber]; !ok {
		te[portName][proto][portNumber] = []discoveryv1.Endpoint{}
	}
	te[portName][proto][portNumber] = append(te[portName][proto][portNumber], endpoint)
}

// newTargetEndpoints constructs a TargetEndpoints structure from a list of EndpointSlices.
// It processes all endpoints from all slices, organizing them by port name, protocol, and port number.
// FQDN address types are skipped.
//
// Parameters:
//   - slices: List of EndpointSlices to process
//
// Returns:
//   - TargetEndpoints: Hierarchically organized endpoint data
func newTargetEndpoints(slices []*discoveryv1.EndpointSlice) targetEndpoints {
	te := targetEndpoints{}

	for _, slice := range slices {
		if slice == nil {
			continue
		}

		if slice.AddressType == discoveryv1.AddressTypeFQDN {
			continue // consider only v4 and v6, discard FQDN
		}

		for _, slicePort := range slice.Ports {
			// Protocol and Port should never be nil per API; thus ignore invalid entries and log.
			if slicePort.Protocol == nil || slicePort.Port == nil {
				klog.Warningf("Skipped invalid slice port %+v belonging to slice %+v", slicePort, slice)
				continue
			}
			portName := getPortName(slicePort.Name)
			for _, endpoint := range slice.Endpoints {
				te.addEndpoint(portName, *slicePort.Protocol, *slicePort.Port, endpoint)
			}
		}
	}
	return te
}

// getPortName safely extracts the port name from a pointer, returning empty string if nil.
// This handles the case where EndpointSlice ports may have unnamed ports.
//
// Parameters:
//   - name: Pointer to port name (may be nil)
//
// Returns:
//   - string: Port name or empty string if nil
func getPortName(name *string) string {
	if name == nil {
		return ""
	}
	return *name
}

// Buckets represents a list of OVS group bucket action strings for load balancing.
// e.g. []string{"ct(commit,table=6)", "ct(commit,table=7)"}.
type Buckets []string

// String formats Buckets into an OVS group bucket specification string.
// e.g. "bucket=bucket_id:0,actions=ct(commit,table=6),bucket=bucket_id:1,actions=ct(commit,table=7)".
func (b Buckets) String() string {
	if len(b) == 0 {
		return ""
	}
	str := fmt.Sprintf("bucket=bucket_id:0,actions=%s", b[0])
	for i := 1; i < len(b); i++ {
		str = fmt.Sprintf("%s,bucket=bucket_id:%d,actions=%s", str, i, b[i])
	}
	return str
}
