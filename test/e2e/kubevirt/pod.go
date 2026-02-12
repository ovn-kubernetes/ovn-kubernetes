package kubevirt

import (
	"encoding/json"
	"fmt"

	infraapi "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	kubevirtv1 "kubevirt.io/api/core/v1"
)

const (
	AddressesAnnotation = "network.kubevirt.io/addresses"
)

func GenerateFakeVirtLauncherPod(namespace, vmName string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "virt-launcher-" + vmName,
			Namespace: namespace,
			Labels: map[string]string{
				kubevirtv1.VirtualMachineNameLabel: vmName,
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:  "compute",
				Image: FakeLauncherImage,
				SecurityContext: &corev1.SecurityContext{
					Privileged: ptr.To(true),
					Capabilities: &corev1.Capabilities{
						Add: []corev1.Capability{"NET_ADMIN"},
					},
				},
			}},
		},
	}
}

func ForceKillVirtLauncherAtNode(p infraapi.Provider, nodeName, vmNamespace, vmName string) error {
	// /usr/bin/virt-launcher --qemu-timeout 312s --name worker-dcf9j --uid bcf975f4-7bdd-4264-948b-b6080320e38a --namespace kv-live-migration-2575 --kubevirt-share-dir /var/run/kubevirt --ephemeral-disk-dir /var/run/kubevirt-ephemeral-disks --container-disk-dir /var/run/kubevirt/container-disks --grace-period-seconds 20 --hook-sidecars 0 --ovmf-path /usr/share/OVMF --run-as-nonroot
	killScript := fmt.Sprintf(`
pid=$(pgrep -f 'virt-launcher .*--name %s.*--namespace %s'|grep -v $$)
ps aux |grep virt-launcher
kill -9 $pid
`, vmName, vmNamespace)
	output, err := p.ExecK8NodeCommand(nodeName, []string{"bash", "-xe", "-c", killScript})
	if err != nil {
		return fmt.Errorf("%s:%w", output, err)
	}
	return nil
}

func GenerateAddressesAnnotations(networkName string, addresses []string) (map[string]string, error) {
	staticIPs, err := json.Marshal(map[string][]string{
		networkName: addresses,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal static IPs: %w", err)
	}
	return map[string]string{
		AddressesAnnotation: string(staticIPs),
	}, nil
}
