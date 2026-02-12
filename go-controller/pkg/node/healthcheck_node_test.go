package node

import (
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/record"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/config"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const healthzAddress string = "127.0.0.1:10256"

var ovnkNodePodName string = "ovnkube-node-test"
var nodeName string = "test-node"

func newFakeOvnkNodePod(deletionTimestamp *metav1.Time) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              ovnkNodePodName,
			UID:               types.UID(ovnkNodePodName),
			Namespace:         config.Kubernetes.OVNConfigNamespace,
			DeletionTimestamp: deletionTimestamp,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "ovnkube-node",
					Image: "ovnkube-image",
				},
			},
			NodeName: nodeName,
		},
	}
}

func initWatchFactoryWithObjects(objects ...runtime.Object) *factory.WatchFactory {
	v1Objects := append([]runtime.Object{}, objects...)
	fakeClient := &util.OVNNodeClientset{
		KubeClient: fake.NewSimpleClientset(v1Objects...),
	}

	watcher, err := factory.NewNodeWatchFactory(fakeClient, nodeName)
	Expect(err).NotTo(HaveOccurred())
	Expect(watcher.Start()).To(Succeed())
	return watcher
}

func checkResponse(address string, expectedStatusCode int) {
	// Try a few times to make sure the server is listening,
	// there's a small race between when Start() returns and
	// the ListenAndServe() is actually active
	var err error
	for i := 0; i < 5; i++ {
		resp, err := http.Get(fmt.Sprintf("http://%s/healthz", address))
		if err == nil {
			defer resp.Body.Close()
			Expect(resp.StatusCode).To(Equal(expectedStatusCode))
		}
		time.Sleep(50 * time.Millisecond)
	}
	Expect(err).NotTo(HaveOccurred())
}

var _ = Describe("Node healthcheck tests", func() {
	var (
		wg           *sync.WaitGroup
		stopCh       chan struct{}
		watchFactory *factory.WatchFactory
	)

	BeforeEach(func() {
		Expect(config.PrepareTestConfig()).To(Succeed())
		stopCh = make(chan struct{})
		wg = &sync.WaitGroup{}
		os.Setenv("POD_NAME", ovnkNodePodName)
	})

	AfterEach(func() {
		close(stopCh)
		wg.Wait()
		watchFactory.Shutdown()
	})

	Context("node proxy healthz server is started", func() {
		It("it reports healthy", func() {
			recorder := record.NewFakeRecorder(10)

			watchFactory = initWatchFactoryWithObjects(
				&corev1.PodList{
					Items: []corev1.Pod{
						*newFakeOvnkNodePod(nil),
					},
				})

			hzs, err := newNodeProxyHealthzServer(nodeName, healthzAddress, recorder, watchFactory)
			Expect(err).NotTo(HaveOccurred())

			hzs.Start(stopCh, wg)

			checkResponse(healthzAddress, http.StatusOK)
		})

		It("it reports unhealthy", func() {
			// ovnk node pod is set for deletion: healthz should report unhealthy
			recorder := record.NewFakeRecorder(10)
			now := metav1.Now()
			watchFactory = initWatchFactoryWithObjects(
				&corev1.PodList{
					Items: []corev1.Pod{
						*newFakeOvnkNodePod(&now),
					},
				})

			hzs, err := newNodeProxyHealthzServer(nodeName, healthzAddress, recorder, watchFactory)
			Expect(err).NotTo(HaveOccurred())

			hzs.Start(stopCh, wg)

			checkResponse(healthzAddress, http.StatusServiceUnavailable)
		})
	})
})
