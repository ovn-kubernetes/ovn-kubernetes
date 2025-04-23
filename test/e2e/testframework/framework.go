package testframework

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/onsi/ginkgo/v2"

	"k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/test/e2e/framework"
	"k8s.io/kubernetes/test/e2e/framework/debug"
	"k8s.io/pod-security-admission/api"

	"github.com/ovn-org/ovn-kubernetes/test/e2e/inclustercommands"
)

var ContainerRuntime = "docker"

func init() {
	if cr, found := os.LookupEnv("CONTAINER_RUNTIME"); found {
		ContainerRuntime = cr
	}
}

// WrappedTestFramework is used to inject OVN specific test actions
func WrappedTestFramework(basename string) *framework.Framework {
	f := NewPrivelegedTestFramework(basename)
	// inject dumping dbs on failure
	ginkgo.JustAfterEach(func() {
		if !ginkgo.CurrentSpecReport().Failed() {
			return
		}

		logLocation := "/var/log"
		dbLocation := "/var/lib/openvswitch"
		// Potential database locations
		ovsdbLocations := []string{"/etc/origin/openvswitch", "/etc/openvswitch"}
		dbs := []string{"ovnnb_db.db", "ovnsb_db.db"}
		ovsdb := "conf.db"

		testName := strings.Replace(ginkgo.CurrentSpecReport().LeafNodeText, " ", "_", -1)
		logDir := fmt.Sprintf("%s/e2e-dbs/%s-%s", logLocation, testName, f.UniqueName)

		var args []string

		// grab all OVS and OVN dbs
		nodes, err := f.ClientSet.CoreV1().Nodes().List(context.TODO(), v1.ListOptions{})
		framework.ExpectNoError(err)
		for _, node := range nodes.Items {
			// ensure e2e-dbs directory with test case exists
			args = []string{ContainerRuntime, "exec", node.Name, "mkdir", "-p", logDir}
			_, err = inclustercommands.RunCommand(args...)
			framework.ExpectNoError(err)

			// Loop through potential OVSDB db locations
			for _, ovsdbLocation := range ovsdbLocations {
				args = []string{ContainerRuntime, "exec", node.Name, "stat", fmt.Sprintf("%s/%s", ovsdbLocation, ovsdb)}
				_, err = inclustercommands.RunCommand(args...)
				if err == nil {
					// node name is the same in kapi and docker
					args = []string{ContainerRuntime, "exec", node.Name, "cp", "-f", fmt.Sprintf("%s/%s", ovsdbLocation, ovsdb),
						fmt.Sprintf("%s/%s", logDir, fmt.Sprintf("%s-%s", node.Name, ovsdb))}
					_, err = inclustercommands.RunCommand(args...)
					framework.ExpectNoError(err)
					break // Stop the loop: the file is found and copied successfully
				}
			}

			// IC will have dbs on every node, but legacy mode wont, check if they exist
			args = []string{ContainerRuntime, "exec", node.Name, "stat", fmt.Sprintf("%s/%s", dbLocation, dbs[0])}
			_, err = inclustercommands.RunCommand(args...)
			if err == nil {
				for _, db := range dbs {
					args = []string{ContainerRuntime, "exec", node.Name, "cp", "-f", fmt.Sprintf("%s/%s", dbLocation, db),
						fmt.Sprintf("%s/%s", logDir, db)}
					_, err = inclustercommands.RunCommand(args...)
					framework.ExpectNoError(err)
				}
			}
		}
	})

	return f
}

func NewPrivelegedTestFramework(basename string) *framework.Framework {
	f := framework.NewDefaultFramework(basename)
	f.NamespacePodSecurityEnforceLevel = api.LevelPrivileged
	f.DumpAllNamespaceInfo = func(ctx context.Context, f *framework.Framework, namespace string) {
		debug.DumpAllNamespaceInfo(context.TODO(), f.ClientSet, namespace)
	}
	return f
}
