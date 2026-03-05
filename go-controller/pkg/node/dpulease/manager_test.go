package dpulease

import (
	"context"
	"testing"
	"time"

	"github.com/onsi/gomega"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestEnsureLeaseCreatesObject(t *testing.T) {
	g := gomega.NewWithT(t)
	client := fake.NewSimpleClientset()
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", UID: types.UID("nodeuid")}}
	mgr := NewManager(client, "ovn-kubernetes", node, 10*time.Second, 40*time.Second)

	lease, err := mgr.EnsureLease(context.Background())
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(lease).NotTo(gomega.BeNil())

	fetched, err := client.CoordinationV1().Leases("ovn-kubernetes").Get(context.Background(), lease.Name, metav1.GetOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(fetched.Spec.HolderIdentity).NotTo(gomega.BeNil())
	g.Expect(*fetched.Spec.HolderIdentity).To(gomega.Equal(HolderIdentity))
	g.Expect(fetched.Spec.LeaseDurationSeconds).NotTo(gomega.BeNil())
	g.Expect(*fetched.Spec.LeaseDurationSeconds).To(gomega.Equal(int32(40)))
	g.Expect(fetched.Spec.RenewTime).NotTo(gomega.BeNil())
	g.Expect(fetched.OwnerReferences).NotTo(gomega.BeEmpty())
	g.Expect(fetched.OwnerReferences[0].UID).To(gomega.Equal(node.UID))

	ready, reason := mgr.Ready()
	g.Expect(ready).To(gomega.BeTrue())
	g.Expect(reason).To(gomega.BeEmpty())
}

func TestRenewUpdatesTimestamp(t *testing.T) {
	g := gomega.NewWithT(t)
	client := fake.NewSimpleClientset()
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", UID: types.UID("nodeuid")}}
	mgr := NewManager(client, "ovn-kubernetes", node, time.Second, 20*time.Second)

	lease, err := mgr.EnsureLease(context.Background())
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(lease.Spec.RenewTime).NotTo(gomega.BeNil())
	originalRenew := lease.Spec.RenewTime.DeepCopy()

	time.Sleep(10 * time.Millisecond)
	g.Expect(mgr.Renew(context.Background())).To(gomega.Succeed())

	updated, err := client.CoordinationV1().Leases("ovn-kubernetes").Get(context.Background(), lease.Name, metav1.GetOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(updated.Spec.RenewTime.Time.After(originalRenew.Time)).To(gomega.BeTrue())
}

func TestCheckStatusDetectsExpiry(t *testing.T) {
	g := gomega.NewWithT(t)
	oldTime := metav1.NewMicroTime(time.Now().Add(-2 * time.Minute))
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ovn-dpu-worker",
			Namespace: "ovn-kubernetes",
		},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity:       ptrToString(HolderIdentity),
			LeaseDurationSeconds: ptrToInt32(10),
			RenewTime:            &oldTime,
		},
	}
	client := fake.NewSimpleClientset(lease)
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", UID: types.UID("nodeuid")}}
	mgr := NewManager(client, "ovn-kubernetes", node, time.Second, 10*time.Second)

	err := mgr.CheckStatus(context.Background())
	g.Expect(err).To(gomega.HaveOccurred())
	ready, reason := mgr.Ready()
	g.Expect(ready).To(gomega.BeFalse())
	g.Expect(reason).To(gomega.ContainSubstring("expired"))
}

func TestCheckStatusHealthy(t *testing.T) {
	g := gomega.NewWithT(t)
	now := metav1.NowMicro()
	lease := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ovn-dpu-worker",
			Namespace: "ovn-kubernetes",
		},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity:       ptrToString(HolderIdentity),
			LeaseDurationSeconds: ptrToInt32(30),
			RenewTime:            &now,
		},
	}
	client := fake.NewSimpleClientset(lease)
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", UID: types.UID("nodeuid")}}
	mgr := NewManager(client, "ovn-kubernetes", node, time.Second, 30*time.Second)

	g.Expect(mgr.CheckStatus(context.Background())).To(gomega.Succeed())
	ready, reason := mgr.Ready()
	g.Expect(ready).To(gomega.BeTrue())
	g.Expect(reason).To(gomega.BeEmpty())
}

func TestEnsureLeaseRetriesOnAlreadyExists(t *testing.T) {
	g := gomega.NewWithT(t)
	client := fake.NewSimpleClientset()
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker", UID: types.UID("nodeuid")}}
	mgr := NewManager(client, "ovn-kubernetes", node, time.Second, 20*time.Second)

	getCalls := 0
	client.Fake.PrependReactor("get", "leases", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		getCalls++
		if getCalls == 1 {
			return true, nil, apierrors.NewNotFound(schema.GroupResource{Group: coordinationv1.GroupName, Resource: "leases"}, "ovn-dpu-worker")
		}
		return false, nil, nil
	})

	createCalls := 0
	client.Fake.PrependReactor("create", "leases", func(_ k8stesting.Action) (bool, runtime.Object, error) {
		createCalls++
		if createCalls == 1 {
			now := metav1.NowMicro()
			existing := &coordinationv1.Lease{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "ovn-dpu-worker",
					Namespace: "ovn-kubernetes",
				},
				Spec: coordinationv1.LeaseSpec{
					HolderIdentity:       ptrToString("someone-else"),
					LeaseDurationSeconds: ptrToInt32(1),
					RenewTime:            &now,
				},
			}
			g.Expect(client.Tracker().Add(existing)).To(gomega.Succeed())
			return true, nil, apierrors.NewAlreadyExists(schema.GroupResource{Group: coordinationv1.GroupName, Resource: "leases"}, "ovn-dpu-worker")
		}
		return false, nil, nil
	})

	lease, err := mgr.EnsureLease(context.Background())
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(lease).NotTo(gomega.BeNil())
	g.Expect(createCalls).To(gomega.Equal(1))
	g.Expect(getCalls).To(gomega.BeNumerically(">=", 2))

	fetched, err := client.CoordinationV1().Leases("ovn-kubernetes").Get(context.Background(), "ovn-dpu-worker", metav1.GetOptions{})
	g.Expect(err).NotTo(gomega.HaveOccurred())
	g.Expect(fetched.Spec.HolderIdentity).NotTo(gomega.BeNil())
	g.Expect(*fetched.Spec.HolderIdentity).To(gomega.Equal(HolderIdentity))
	g.Expect(fetched.Spec.LeaseDurationSeconds).NotTo(gomega.BeNil())
	g.Expect(*fetched.Spec.LeaseDurationSeconds).To(gomega.Equal(int32(20)))
}

func ptrToString(val string) *string {
	return &val
}

func ptrToInt32(val int32) *int32 {
	return &val
}
