package kubevirt

import (
	"fmt"
	"net"
	"sort"
	"strings"

	kubevirtv1 "kubevirt.io/api/core/v1"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ktypes "k8s.io/apimachinery/pkg/types"
	v1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/util/retry"

	libovsdbclient "github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/generator/udn"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/kube"
	libovsdbops "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	logicalswitchmanager "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/logical_switch_manager"
	ovntypes "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/ndp"
)

// DefaultGatewayReconciler is responsible for reconciling the default gateway
// configuration of a virtual machine's network interface after a live migration.
// It supports both IPv4 and IPv6 configurations.
type DefaultGatewayReconciler struct {
	watchFactory  *factory.WatchFactory
	netInfo       util.NetInfo
	interfaceName string
}

// NewDefaultGatewayReconciler creates a new instance of DefaultGatewayReconciler.
// It takes a WatchFactory for managing resource watches, a NetInfo object for network information,
// and the name of the network interface to send ARPs or RAs as parameters.
func NewDefaultGatewayReconciler(watchFactory *factory.WatchFactory, netInfo util.NetInfo, interfaceName string) *DefaultGatewayReconciler {
	return &DefaultGatewayReconciler{
		watchFactory:  watchFactory,
		netInfo:       netInfo,
		interfaceName: interfaceName,
	}
}

// IsPodLiveMigratable will return true if the pod belongs
// to kubevirt and should use the live migration features
func IsPodLiveMigratable(pod *corev1.Pod) bool {
	_, ok := pod.Annotations[kubevirtv1.AllowPodBridgeNetworkLiveMigrationAnnotation]
	return ok
}

// TODO: remove adapter once all findVMRelatedPods usages transition to use PodLister
type listPodsFn func(namespace string, selector metav1.LabelSelector) ([]*corev1.Pod, error)

// findVMRelatedPods will return pods belong to the same vm annotated at pod and
// filter out the one at the function argument
func findVMRelatedPods(client *factory.WatchFactory, pod *corev1.Pod) ([]*corev1.Pod, error) {
	return findVMRelatedPodsWithListerFn(client.GetPodsBySelector, pod)
}

func findVMRelatedPodsWithListerFn(listPodsFn listPodsFn, pod *corev1.Pod) ([]*corev1.Pod, error) {
	vmName, ok := pod.Labels[kubevirtv1.VirtualMachineNameLabel]
	if !ok {
		return nil, nil
	}
	vmLabelSelector := metav1.LabelSelector{MatchLabels: map[string]string{kubevirtv1.VirtualMachineNameLabel: vmName}}
	vmPods, err := listPodsFn(pod.Namespace, vmLabelSelector)
	if err != nil {
		return nil, err
	}
	if len(vmPods) == 0 {
		return []*corev1.Pod{}, nil
	}

	filteredOutVMPods := []*corev1.Pod{}
	for _, vmPod := range vmPods {
		// The purpose of this function is to return the "other" pods related
		// to a VM.
		if vmPod.UID == pod.UID {
			continue
		}
		filteredOutVMPods = append(filteredOutVMPods, vmPod)
	}

	return filteredOutVMPods, nil
}

// findPodAnnotation will return the the OVN pod
// annotation from any other pod annotated with the same VM as pod
func findPodAnnotation(client *factory.WatchFactory, pod *corev1.Pod, nadKey string) (*util.PodAnnotation, error) {
	vmPods, err := findVMRelatedPods(client, pod)
	if err != nil {
		return nil, fmt.Errorf("failed finding related pods for pod %s/%s when looking for network info: %v", pod.Namespace, pod.Name, err)
	}
	// virtual machine is not being live migrated so there is no other
	// vm pods
	if len(vmPods) == 0 {
		return nil, nil
	}

	for _, vmPod := range vmPods {
		podAnnotation, err := util.UnmarshalPodAnnotation(vmPod.Annotations, nadKey)
		if err == nil {
			return podAnnotation, nil
		}
	}
	return nil, fmt.Errorf("missing virtual machine pod annotation at stale pods for %s/%s", pod.Namespace, pod.Name)
}

// EnsurePodAnnotationForVM will at live migration extract the ovn pod
// annotations from the source vm pod and copy it
// to the target vm pod so ip address follow vm during migration. This has to
// done before creating the LSP to be sure that Address field get configured
// correctly at the target VM pod LSP.
func EnsurePodAnnotationForVM(watchFactory *factory.WatchFactory, kube *kube.KubeOVN, pod *corev1.Pod, nadKey string) (*util.PodAnnotation, error) {
	if !IsPodLiveMigratable(pod) {
		return nil, nil
	}

	if podAnnotation, err := util.UnmarshalPodAnnotation(pod.Annotations, nadKey); err == nil {
		return podAnnotation, nil
	}

	podAnnotation, err := findPodAnnotation(watchFactory, pod, nadKey)
	if err != nil {
		return nil, err
	}
	if podAnnotation == nil {
		return nil, nil
	}

	var modifiedPod *corev1.Pod
	resultErr := retry.RetryOnConflict(util.OvnConflictBackoff, func() error {
		// Informer cache should not be mutated, so get a copy of the object
		pod, err := watchFactory.GetPod(pod.Namespace, pod.Name)
		if err != nil {
			return err
		}
		// Informer cache should not be mutated, so get a copy of the object
		modifiedPod = pod.DeepCopy()
		if podAnnotation != nil {
			modifiedPod.Annotations, err = util.MarshalPodAnnotation(modifiedPod.Annotations, podAnnotation, nadKey)
			if err != nil {
				return err
			}
		}
		return kube.UpdatePodStatus(modifiedPod)
	})
	if resultErr != nil {
		return nil, fmt.Errorf("failed to update labels and annotations on pod %s/%s: %v", pod.Namespace, pod.Name, resultErr)
	}
	return podAnnotation, nil
}

// AllVMPodsAreCompleted return true if all the vm pods are completed
func AllVMPodsAreCompleted(podLister v1.PodLister, pod *corev1.Pod) (bool, error) {
	if !util.PodCompleted(pod) {
		return false, nil
	}

	f := func(namespace string, selector metav1.LabelSelector) ([]*corev1.Pod, error) {
		s, err := metav1.LabelSelectorAsSelector(&selector)
		if err != nil {
			return nil, err
		}
		return podLister.Pods(namespace).List(s)
	}
	vmPods, err := findVMRelatedPodsWithListerFn(f, pod)
	if err != nil {
		return false, fmt.Errorf("failed finding related pods for pod %s/%s when checking if they are completed: %v", pod.Namespace, pod.Name, err)
	}

	for _, vmPod := range vmPods {
		if !util.PodCompleted(vmPod) {
			return false, nil
		}
	}
	return true, nil
}

// IsMigratedSourcePodStale return false if the pod is live migratable,
// not completed and is the running VM pod with newest creation timestamp
func IsMigratedSourcePodStale(client *factory.WatchFactory, pod *corev1.Pod) (bool, error) {
	if !IsPodLiveMigratable(pod) {
		return false, nil
	}

	if util.PodCompleted(pod) {
		return true, nil
	}

	vmPods, err := findVMRelatedPods(client, pod)
	if err != nil {
		return false, fmt.Errorf("failed finding related pods for pod %s/%s when checking live migration left overs: %v", pod.Namespace, pod.Name, err)
	}

	for _, vmPod := range vmPods {
		if vmPod.CreationTimestamp.After(pod.CreationTimestamp.Time) {
			return true, nil
		}
	}

	return false, nil
}

// ZoneContainsPodSubnet will return true if the logical switch tonains
// the pod subnet and also the switch name owning it, this means that
// this zone owns the that subnet.
func ZoneContainsPodSubnet(lsManager *logicalswitchmanager.LogicalSwitchManager, ips []*net.IPNet) (string, bool) {
	return lsManager.GetSubnetName(ips)
}

// nodeContainsPodSubnet will return true if the node subnet annotation
// contains the subnets from the argument
func nodeContainsPodSubnet(watchFactory *factory.WatchFactory, nodeName string, podAnnotation *util.PodAnnotation, netName string) (bool, error) {
	node, err := watchFactory.GetNode(nodeName)
	if err != nil {
		return false, err
	}
	nodeHostSubNets, err := util.ParseNodeHostSubnetAnnotation(node, netName)
	if err != nil {
		return false, err
	}
	for _, subnet := range podAnnotation.IPs {
		for _, nodeHostSubNet := range nodeHostSubNets {
			if nodeHostSubNet.Contains(subnet.IP) {
				return true, nil
			}
		}
	}
	return false, nil
}

// ExtractVMNameFromPod returns namespace and name of vm backed up but the pod
// for regular pods return nil
func ExtractVMNameFromPod(pod *corev1.Pod) *ktypes.NamespacedName {
	vmName, ok := pod.Labels[kubevirtv1.VirtualMachineNameLabel]
	if !ok {
		return nil
	}
	return &ktypes.NamespacedName{Namespace: pod.Namespace, Name: vmName}
}

// CleanUpLiveMigratablePod remove routing and DHCP ovn related resources
// when all the pods for the same VM as `pod` argument are completed.
func CleanUpLiveMigratablePod(nbClient libovsdbclient.Client, watchFactory *factory.WatchFactory, pod *corev1.Pod) error {
	if !IsPodLiveMigratable(pod) {
		return nil
	}

	allVMPodsCompleted, err := AllVMPodsAreCompleted(watchFactory.PodCoreInformer().Lister(), pod)
	if err != nil {
		return fmt.Errorf("failed cleaning up VM when checking if pod is leftover: %v", err)
	}

	// Do cleanup only if all the pods related to the VM are completed
	if !allVMPodsCompleted {
		return nil
	}

	if err := DeleteDHCPOptions(nbClient, pod); err != nil {
		return err
	}
	if err := DeleteRoutingForMigratedPod(nbClient, pod); err != nil {
		return err
	}
	return nil
}

func SyncVirtualMachines(nbClient libovsdbclient.Client, vms map[ktypes.NamespacedName]bool) error {
	if err := libovsdbops.DeleteLogicalRouterStaticRoutesWithPredicate(nbClient, ovntypes.OVNClusterRouter, func(item *nbdb.LogicalRouterStaticRoute) bool {
		return ownsItAndIsOrphanOrWrongZone(item.ExternalIDs, vms)
	}); err != nil {
		return fmt.Errorf("failed deleting stale vm static routes: %v", err)
	}
	if err := libovsdbops.DeleteLogicalRouterPoliciesWithPredicate(nbClient, ovntypes.OVNClusterRouter, func(item *nbdb.LogicalRouterPolicy) bool {
		return ownsItAndIsOrphanOrWrongZone(item.ExternalIDs, vms)
	}); err != nil {
		return fmt.Errorf("failed deleting stale vm policies: %v", err)
	}
	if err := libovsdbops.DeleteDHCPOptionsWithPredicate(nbClient, func(item *nbdb.DHCPOptions) bool {
		return ownsItAndIsOrphanOrWrongZone(item.ExternalIDs, vms)
	}); err != nil {
		return fmt.Errorf("failed deleting stale dhcp options: %v", err)
	}
	return nil
}

// FindLiveMigratablePods will return all the pods with a `vm.kubevirt.io`
// label filtered by `kubevirt.io/allow-pod-bridge-network-live-migration`
// annotation
func FindLiveMigratablePods(watchFactory *factory.WatchFactory) ([]*corev1.Pod, error) {
	vmPods, err := watchFactory.GetAllPodsBySelector(
		metav1.LabelSelector{
			MatchExpressions: []metav1.LabelSelectorRequirement{{
				Key:      kubevirtv1.VirtualMachineNameLabel,
				Operator: metav1.LabelSelectorOpExists,
			}},
		},
	)
	if err != nil {
		return nil, fmt.Errorf("failed looking for live migratable pods: %v", err)
	}
	liveMigratablePods := []*corev1.Pod{}
	for _, vmPod := range vmPods {
		if IsPodLiveMigratable(vmPod) {
			liveMigratablePods = append(liveMigratablePods, vmPod)
		}
	}
	return liveMigratablePods, nil
}

// allocateSyncMigratablePodIPs will refill ip pool in
// case the node has take over the vm subnet for live migrated vms
func allocateSyncMigratablePodIPs(watchFactory *factory.WatchFactory, lsManager *logicalswitchmanager.LogicalSwitchManager, nodeName, nadKey string, pod *corev1.Pod, allocatePodIPsOnSwitch func(*corev1.Pod, *util.PodAnnotation, string, string) (string, error)) (*ktypes.NamespacedName, string, *util.PodAnnotation, error) {
	isStale, err := IsMigratedSourcePodStale(watchFactory, pod)
	if err != nil {
		return nil, "", nil, err
	}

	// We care only for Running virt-launcher pods
	if isStale {
		return nil, "", nil, nil
	}

	vmKey := ExtractVMNameFromPod(pod)

	annotation, err := util.UnmarshalPodAnnotation(pod.Annotations, nadKey)
	if err != nil {
		return nil, "", nil, nil
	}
	switchName, zoneContainsPodSubnet := ZoneContainsPodSubnet(lsManager, annotation.IPs)
	// If this zone do not own the subnet or the node that is passed
	// do not match the switch, they should not be deallocated
	if !zoneContainsPodSubnet || (nodeName != "" && switchName != nodeName) {
		return vmKey, "", annotation, nil
	}
	expectedLogicalPortName, err := allocatePodIPsOnSwitch(pod, annotation, nadKey, switchName)
	if err != nil {
		return vmKey, "", nil, err
	}
	return vmKey, expectedLogicalPortName, annotation, nil
}

// AllocateSyncMigratablePodIPsOnZone will refill ip pool in
// with pod's IPs if those IPs belong to the zone
func AllocateSyncMigratablePodIPsOnZone(watchFactory *factory.WatchFactory, lsManager *logicalswitchmanager.LogicalSwitchManager, nadKey string, pod *corev1.Pod, allocatePodIPsOnSwitch func(*corev1.Pod, *util.PodAnnotation, string, string) (string, error)) (*ktypes.NamespacedName, string, *util.PodAnnotation, error) {
	// We care about the whole zone so we pass the nodeName empty
	return allocateSyncMigratablePodIPs(watchFactory, lsManager, "", nadKey, pod, allocatePodIPsOnSwitch)
}

// ZoneContainsPodSubnetOrUntracked returns whether a pod with its corresponding
// allocated IPs as reflected on the annotation come from a subnet that is
// either assigned to a node of the zone or, not assigned to any node after
// migrating from a node that has since been deleted and the subnet originally
// assigned to that node has not yet been re-assigned to a different node. For
// convenience, the host subnets might not provided in which case they might be
// parsed and returned if used.
func ZoneContainsPodSubnetOrUntracked(watchFactory *factory.WatchFactory, lsManager *logicalswitchmanager.LogicalSwitchManager, hostSubnets []*net.IPNet, annotation *util.PodAnnotation) ([]*net.IPNet, bool, error) {
	_, local := ZoneContainsPodSubnet(lsManager, annotation.IPs)
	if local {
		return nil, true, nil
	}
	if len(hostSubnets) == 0 {
		nodes, err := watchFactory.GetNodes()
		if err != nil {
			return nil, false, err
		}
		hostSubnets, err = util.ParseNodesHostSubnetAnnotation(nodes, ovntypes.DefaultNetworkName)
		if err != nil {
			return nil, false, err
		}
	}
	// we can just use one of the IPs to check if it belongs to a subnet assigned
	// to a node
	return hostSubnets, !util.IsContainedInAnyCIDR(annotation.IPs[0], hostSubnets...), nil
}

// IsPodOwnedByVirtualMachine returns true if the pod is own by a
// kubevirt virtual machine, false otherwise.
func IsPodOwnedByVirtualMachine(pod *corev1.Pod) bool {
	return ExtractVMNameFromPod(pod) != nil
}

// IsPodAllowedForMigration determines whether a given pod is eligible for live migration
func IsPodAllowedForMigration(pod *corev1.Pod, netInfo util.NetInfo) bool {
	return IsPodOwnedByVirtualMachine(pod) &&
		(netInfo.TopologyType() == ovntypes.Layer2Topology ||
			netInfo.TopologyType() == ovntypes.LocalnetTopology)
}

func isTargetPodReady(targetPod *corev1.Pod) bool {
	if targetPod == nil {
		return false
	}

	// This annotation only appears on live migration scenarios, and it signals
	// that target VM pod is ready to receive traffic, so we can route
	// traffic to it.
	targetReadyTimestamp := targetPod.Annotations[kubevirtv1.MigrationTargetReadyTimestamp]

	// VM is ready to receive traffic
	return targetReadyTimestamp != ""
}

func filterNotComplete(vmPods []*corev1.Pod) []*corev1.Pod {
	var notCompletePods []*corev1.Pod
	for _, vmPod := range vmPods {
		if !util.PodCompleted(vmPod) {
			notCompletePods = append(notCompletePods, vmPod)
		}
	}

	return notCompletePods
}

func tooManyPodsError(livingPods []*corev1.Pod) error {
	var podNames = make([]string, len(livingPods))
	for i := range livingPods {
		podNames[i] = livingPods[i].Namespace + "/" + livingPods[i].Name
	}
	return fmt.Errorf("unexpected live migration state at pods: %s", strings.Join(podNames, ","))
}

// LiveMigrationState represents the various states of a live migration process.
type LiveMigrationState string

const (
	// LiveMigrationInProgress indicates that a live migration is currently ongoing.
	LiveMigrationInProgress LiveMigrationState = "InProgress"

	// LiveMigrationTargetDomainReady indicates that the target domain is ready to take over.
	LiveMigrationTargetDomainReady LiveMigrationState = "TargetDomainReady"

	// LiveMigrationFailed indicates that the live migration process has failed.
	LiveMigrationFailed LiveMigrationState = "Failed"
)

// LiveMigrationStatus provides details about the current status of a live migration.
// It includes information about the source and target pods as well as the migration state.
type LiveMigrationStatus struct {
	SourcePod *corev1.Pod        // SourcePod is the original pod.
	TargetPod *corev1.Pod        // TargetPod is the destination pod.
	State     LiveMigrationState // State is the current state of the live migration.
}

// IsTargetDomainReady returns true if the target domain in the live migration process is ready.
func (lm LiveMigrationStatus) IsTargetDomainReady() bool {
	return lm.State == LiveMigrationTargetDomainReady
}

// DiscoverLiveMigrationStatus determines the status of a live migration for a given pod.
// It analyzes the state of pods associated with a VirtualMachine (VM) to identify whether
// a live migration is in progress, the target domain is ready, or the migration has failed.
//
// Note: The function assumes that the pod is part of a VirtualMachine resource managed
// by KubeVirt.
func DiscoverLiveMigrationStatus(client *factory.WatchFactory, pod *corev1.Pod) (*LiveMigrationStatus, error) {
	vmKey := ExtractVMNameFromPod(pod)
	if vmKey == nil {
		return nil, nil
	}

	vmPods, err := client.GetPodsBySelector(pod.Namespace, metav1.LabelSelector{MatchLabels: map[string]string{kubevirtv1.VirtualMachineNameLabel: vmKey.Name}})
	if err != nil {
		return nil, err
	}

	// no migration
	if len(vmPods) < 2 {
		return nil, nil
	}

	// Sort vmPods by creation time
	sort.Slice(vmPods, func(i, j int) bool {
		return vmPods[j].CreationTimestamp.After(vmPods[i].CreationTimestamp.Time)
	})

	targetPod := vmPods[len(vmPods)-1]
	livingPods := filterNotComplete(vmPods)

	// If there is no living pod we should state no live migration status
	if len(livingPods) == 0 {
		return nil, nil
	}

	// There is a living pod but is not the target one so the migration
	// has failed.
	if util.PodCompleted(targetPod) {
		return &LiveMigrationStatus{
			SourcePod: livingPods[0],
			TargetPod: targetPod,
			State:     LiveMigrationFailed,
		}, nil
	}

	// no active migration
	if len(livingPods) < 2 {
		return nil, nil
	}

	if len(livingPods) > 2 {
		return nil, tooManyPodsError(livingPods)
	}

	status := LiveMigrationStatus{
		SourcePod: livingPods[0],
		TargetPod: livingPods[1],
		State:     LiveMigrationInProgress,
	}

	if isTargetPodReady(status.TargetPod) {
		status.State = LiveMigrationTargetDomainReady
	}
	return &status, nil
}

// ReconcileIPv4AfterLiveMigration will send a GARP after live migration
// to update the default gw mac address to the node where the VM is running
// now.
func (r *DefaultGatewayReconciler) ReconcileIPv4AfterLiveMigration(liveMigrationStatus *LiveMigrationStatus) error {
	if liveMigrationStatus.State != LiveMigrationTargetDomainReady {
		return nil
	}
	var gwMAC net.HardwareAddr
	if !config.Layer2UsesTransitRouter {
		targetNode, err := r.watchFactory.GetNode(liveMigrationStatus.TargetPod.Spec.NodeName)
		if err != nil {
			return err
		}

		lrpJoinAddress, err := udn.GetGWRouterIPv4(targetNode, r.netInfo)
		if err != nil {
			return err
		}

		gwMAC = util.IPAddrToHWAddr(lrpJoinAddress)
	}
	for _, subnet := range r.netInfo.Subnets() {
		gwIP := r.netInfo.GetNodeGatewayIP(subnet.CIDR).IP.To4()
		if gwIP == nil {
			continue
		}
		if config.Layer2UsesTransitRouter {
			gwMAC = util.IPAddrToHWAddr(gwIP)
		}
		garp, err := util.NewGARP(gwIP, &gwMAC)
		if err != nil {
			return fmt.Errorf("failed to create GARP for gateway IP %s: %w", gwIP, err)
		}
		if err := util.BroadcastGARP(r.interfaceName, garp); err != nil {
			return err
		}
	}
	return nil
}

// ReconcileIPv6AfterLiveMigration will do two things at VM's:
// - Remove ipv6 default gw path from VM's node before live migration
// - Add ipv6 default gw path from VM's node after live migration
// This is done by sending a pair of unsolicited RA's one with lifetime=0
// (to remove the gateway path) another with lifetime=max to add the new
// default gateway path
func (r *DefaultGatewayReconciler) ReconcileIPv6AfterLiveMigration(liveMigration *LiveMigrationStatus) error {
	if !liveMigration.IsTargetDomainReady() {
		return nil
	}
	nodes, err := r.watchFactory.GetNodes()
	if err != nil {
		return err
	}

	targetPod := liveMigration.TargetPod
	if len(r.netInfo.GetNADs()) != 1 {
		return fmt.Errorf("expected only one nad for network %q, got %d", r.netInfo.GetNetworkName(), len(r.netInfo.GetNADs()))
	}

	targetPodAnnotation, err := util.UnmarshalPodAnnotation(targetPod.Annotations, r.netInfo.GetNADs()[0])
	if err != nil {
		return ovntypes.NewSuppressedError(fmt.Errorf("failed parsing ovn pod annotation for pod '%s/%s' and network %q: %w", targetPod.Namespace, targetPod.Name, r.netInfo.GetNetworkName(), err))
	}

	destinationIP, err := util.MatchFirstIPNetFamily(true /* ipv6 */, targetPodAnnotation.IPs)
	if err != nil {
		return err
	}
	destinationMAC := targetPodAnnotation.MAC

	ras := make([]ndp.RouterAdvertisement, 0, len(nodes))
	for _, node := range nodes {
		if !config.Layer2UsesTransitRouter && node.Name == liveMigration.TargetPod.Spec.NodeName {
			// skip the target node since this is the proper gateway
			continue
		}
		nodeJoinAddrs, err := udn.GetGWRouterIPs(node, r.netInfo)
		if err != nil {
			return ovntypes.NewSuppressedError(fmt.Errorf("failed parsing join addresss from node %q and network %q to reconcile ipv6 gateway: %w", node.Name, r.netInfo.GetNetworkName(), err))
		}
		// During upgrades, nftables blocks Router Advertisements (RAs) from other nodes.
		// However, Virtual Machines (VMs) may still retain old default gateway paths.
		// To address this, we create a new Router Advertisement with a lifetime of 0
		// to signal the removal of the old default gateway.
		// NOTE: This is a workaround for the issue and may not be needed in the future, after
		//       upgrading to a version that supports the new behavior.
		ras = append(ras, newRouterAdvertisementFromIPAndLifetime(nodeJoinAddrs[0].IP, destinationMAC, destinationIP.IP, 0))
	}
	if !config.Layer2UsesTransitRouter {
		targetNode, err := r.watchFactory.GetNode(liveMigration.TargetPod.Spec.NodeName)
		if err != nil {
			return fmt.Errorf("failed fetching node %q to reconcile ipv6 gateway: %w", liveMigration.TargetPod.Spec.NodeName, err)
		}
		targetNodeJoinAddrs, err := udn.GetGWRouterIPs(targetNode, r.netInfo)
		if err != nil {
			return ovntypes.NewSuppressedError(fmt.Errorf("failed parsing join addresss from live migration target node %q and network %q to reconcile ipv6 gateway: %w", targetNode.Name, r.netInfo.GetNetworkName(), err))
		}
		ras = append(ras, newRouterAdvertisementFromIPAndLifetime(targetNodeJoinAddrs[0].IP, destinationMAC, destinationIP.IP, 65535))
	} else {
		if len(targetPodAnnotation.Gateways) == 0 {
			return fmt.Errorf("missing gateways to calculate ipv6 gateway reconciler RA")
		}
		// The LRP mac is calculated from the first address on the list.
		gwIP := targetPodAnnotation.Gateways[0]

		// Create Prefix Information Option with IPv6 join subnet
		prefixNet := r.netInfo.JoinSubnetV6()
		if prefixNet == nil {
			return fmt.Errorf("no IPv6 join subnet available for network %q", r.netInfo.GetNetworkName())
		}

		prefixInfo := ndp.PrefixInformation{
			Prefix:            *prefixNet,
			ValidLifetime:     0,
			PreferredLifetime: 0, // IP lifetime 0 as requested
			OnLink:            true,
			Autonomous:        true,
		}

		ras = append(ras, newRouterAdvertisementWithPrefixInfos(gwIP, destinationMAC, destinationIP.IP, 65535, []ndp.PrefixInformation{prefixInfo}))
	}

	return ndp.SendRouterAdvertisements(r.interfaceName, ras...)
}

// newRouterAdvertisementFromIPAndLifetime creates a new Router Advertisement (RA) message
// using the provided IP address, destination MAC, destination IP, and lifetime.
//
// This function performs the following:
// - Derives the source MAC address from the given IP using util.IPAddrToHWAddr.
// - Calculates the link-local address (LLA) from the source MAC using util.HWAddrToIPv6LLA.
// - Configures the destination IP and MAC address to use the provided values.
// - Sets the RA message's lifetime to the specified value.
//
// Parameters:
// - ip: The IP address used to derive the source MAC and LLA.
// - destinationMAC: The MAC address to which the RA message will be sent.
// - destinationIP: The IP address to which the RA message will be sent.
// - lifetime: The lifetime value for the RA message, in seconds.
//
// Returns:
// - An ndp.RouterAdvertisement object configured with the calculated source MAC, LLA, and the provided destination MAC, IP, and lifetime.
func newRouterAdvertisementFromIPAndLifetime(ip net.IP, destinationMAC net.HardwareAddr, destinationIP net.IP, lifetime uint16) ndp.RouterAdvertisement {
	sourceMAC := util.IPAddrToHWAddr(ip)
	return ndp.RouterAdvertisement{
		SourceMAC:      sourceMAC,
		SourceIP:       util.HWAddrToIPv6LLA(sourceMAC),
		DestinationMAC: destinationMAC,
		DestinationIP:  destinationIP,
		Lifetime:       lifetime,
	}
}

func newRouterAdvertisementWithPrefixInfos(ip net.IP, destinationMAC net.HardwareAddr, destinationIP net.IP, lifetime uint16, prefixInfos []ndp.PrefixInformation) ndp.RouterAdvertisement {
	sourceMAC := util.IPAddrToHWAddr(ip)
	return ndp.RouterAdvertisement{
		SourceMAC:      sourceMAC,
		SourceIP:       util.HWAddrToIPv6LLA(sourceMAC),
		DestinationMAC: destinationMAC,
		DestinationIP:  destinationIP,
		Lifetime:       lifetime,
		PrefixInfos:    prefixInfos,
	}
}
