// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package diagnostics

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/kubernetes/test/e2e/framework"
)

// MigrationCapture follows the launcher and HTTP pod interfaces so captures
// retain Ethernet addresses on both sides of a migration.
func (d *Diagnostics) MigrationCapture() (func(bool), error) {
	if os.Getenv("KUBEVIRT_MIGRATION_DEBUG") != "true" {
		return func(bool) {}, nil
	}
	namespace := d.fr.Namespace.Name
	dir := "/var/log/kubevirt-migration-debug/" + namespace
	cmd := fmt.Sprintf(`set -eu
mkdir -p /host%[1]s
cd /host%[1]s
exec > >(tee -a collector.log) 2>&1
pids=()
finish() {
  if (("${#pids[@]}")); then
    kill -INT "${pids[@]}" 2>/dev/null || true
    wait "${pids[@]}" 2>/dev/null || true
  fi
  touch stopped
}
trap finish EXIT
trap 'exit 0' TERM INT
command -v tcpdump
command -v ovs-vsctl
command -v ovs-ofctl
# Per-interface captures preserve the destination MAC, which Linux cooked
# captures on "any" can omit.
filter='arp or icmp6 or tcp port 8000'
tcpdump -Z root -U -nn -i any -s 192 -C 8 -W 2 -w node.pcap "$filter" 2>node.stderr &
pids+=("$!")
sleep 1
kill -0 "${pids[0]}"
declare -A captures=()
while [ ! -e stop ]; do
  kill -0 "${pids[0]}"
  # Bare data keeps OVS from quoting interface names that start with a digit.
  interfaces=$(ovs-vsctl --timeout=5 --format=csv --data=bare --no-headings --columns=name,external_ids list Interface)
  while IFS=, read -r iface metadata; do
    case "$metadata" in
      *"iface-id=%[2]s_virt-launcher-"*|*"iface-id=%[2]s_testpod-"*) ;;
      *) continue ;;
    esac
    [ -z "${captures[$iface]:-}" ] || continue
    ip link show dev "$iface" >/dev/null 2>&1 || continue
    date -u --iso-8601=ns
    echo "$iface,$metadata"
    tcpdump -Z root -U -nn -i "$iface" -s 192 -C 8 -W 2 -w "$iface.pcap" "$filter" 2>"$iface.stderr" &
    captures[$iface]=$!
    pids+=("$!")
  done <<< "$interfaces"
  touch ready
  sleep 1
done
if [ -e failed ]; then
  date -u --iso-8601=ns
  timeout 10 ovs-ofctl -O OpenFlow15 dump-flows br-int > br-int.flows || true
  ovs-vsctl --timeout=5 --format=json list Interface > interfaces.json || true
fi
`, dir, namespace)
	ds := d.composeDiagnosticsDaemonSet("migration-capture", cmd, "migration-capture")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	_, err := d.fr.ClientSet.AppsV1().DaemonSets(namespace).Create(ctx, &ds, metav1.CreateOptions{})
	if err != nil {
		return nil, err
	}
	err = wait.PollUntilContextCancel(ctx, time.Second, true, func(ctx context.Context) (bool, error) {
		current, err := d.fr.ClientSet.AppsV1().DaemonSets(namespace).Get(ctx, ds.Name, metav1.GetOptions{})
		return err == nil && current.Status.DesiredNumberScheduled > 0 &&
			current.Status.NumberReady == current.Status.DesiredNumberScheduled, err
	})
	if err != nil {
		return nil, err
	}
	pods, err := d.fr.ClientSet.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: "app=migration-capture"})
	if err != nil {
		return nil, err
	}
	for _, pod := range pods.Items {
		err = wait.PollUntilContextCancel(ctx, time.Second, true, func(context.Context) (bool, error) {
			_, err := infraprovider.Get().ExecK8NodeCommand(pod.Spec.NodeName, []string{"test", "-f", dir + "/ready"})
			return err == nil, nil
		})
		if err != nil {
			return nil, fmt.Errorf("waiting for migration capture on %s: %w", pod.Spec.NodeName, err)
		}
	}
	return func(passed bool) {
		for _, pod := range pods.Items {
			_, err := infraprovider.Get().ExecK8NodeCommand(pod.Spec.NodeName, []string{"bash", "-c", `set -eu
cd "$1"
if [ "$2" = false ]; then touch failed; fi
touch stop
timeout 20 bash -c 'until [ -e stopped ]; do sleep 1; done'
if [ "$2" = true ]; then rm -rf -- "$1"; fi
`, "stop-migration-capture", dir, fmt.Sprint(passed)})
			if err != nil {
				framework.Logf("Stopping migration capture on %s: %v", pod.Spec.NodeName, err)
			}
		}
	}, nil
}
