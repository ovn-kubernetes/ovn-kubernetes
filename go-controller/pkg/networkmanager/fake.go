package networkmanager

import (
	"context"
	"fmt"
	"sync"

	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util/errors"
)

type FakeNetworkController struct {
	util.NetInfo
}

func (fnc *FakeNetworkController) Start(_ context.Context) error {
	return nil
}

func (fnc *FakeNetworkController) Stop() {}

func (fnc *FakeNetworkController) Cleanup() error {
	return nil
}

func (nc *FakeNetworkController) Reconcile(util.NetInfo) error {
	return nil
}

type FakeControllerManager struct{}

func (fcm *FakeControllerManager) NewNetworkController(netInfo util.NetInfo) (NetworkController, error) {
	return &FakeNetworkController{netInfo}, nil
}

func (fcm *FakeControllerManager) CleanupStaleNetworks(_ ...util.NetInfo) error {
	return nil
}

func (fcm *FakeControllerManager) GetDefaultNetworkController() ReconcilableNetworkController {
	return nil
}

func (fcm *FakeControllerManager) Reconcile(_ string, _, _ util.NetInfo) error {
	return nil
}

type FakeNetworkManager struct {
	sync.Mutex
	// namespace -> netInfo
	// if netInfo is nil, it represents a namespace which contains the required UDN label but with no valid network. It will return invalid network error.
	PrimaryNetworks map[string]util.NetInfo
	Reconcilers     []reconcilerRegistration
	nextID          uint64
	// UDNNamespaces are a list of namespaces that require UDN for primary network
	UDNNamespaces sets.Set[string]
}

func (fnm *FakeNetworkManager) RegisterNADReconciler(r NADReconciler) (uint64, error) {
	fnm.Lock()
	defer fnm.Unlock()
	fnm.nextID++
	id := fnm.nextID
	fnm.Reconcilers = append(fnm.Reconcilers, reconcilerRegistration{id: id, r: r})
	return id, nil
}

func (fnm *FakeNetworkManager) DeRegisterNADReconciler(id uint64) error {
	fnm.Lock()
	defer fnm.Unlock()
	for i, rec := range fnm.Reconcilers {
		if rec.id == id {
			fnm.Reconcilers = append(fnm.Reconcilers[:i], fnm.Reconcilers[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("reconciler not found")
}

func (fnm *FakeNetworkManager) TriggerHandlers(nadName string, info util.NetInfo, removed bool) {
	fnm.Lock()
	defer fnm.Unlock()
	_ = info
	_ = removed
	for _, entry := range fnm.Reconcilers {
		entry.r.Reconcile(nadName)
	}
}

func (fnm *FakeNetworkManager) Interface() Interface {
	return fnm
}

func (fnm *FakeNetworkManager) Start() error { return nil }

func (fnm *FakeNetworkManager) Stop() {}

func (fnm *FakeNetworkManager) GetActiveNetworkForNamespace(namespace string) (util.NetInfo, error) {
	network := fnm.GetActiveNetworkForNamespaceFast(namespace)
	if network == nil {
		return nil, util.NewInvalidPrimaryNetworkError(namespace)
	}
	return network, nil
}

func (fnm *FakeNetworkManager) GetActiveNetworkForNamespaceFast(namespace string) util.NetInfo {
	fnm.Lock()
	defer fnm.Unlock()
	if primaryNetworks, ok := fnm.PrimaryNetworks[namespace]; ok {
		return primaryNetworks
	}
	if fnm.UDNNamespaces != nil && fnm.UDNNamespaces.Has(namespace) {
		return nil
	}
	return &util.DefaultNetInfo{}
}

func (fnm *FakeNetworkManager) GetNetwork(networkName string) util.NetInfo {
	for _, ni := range fnm.PrimaryNetworks {
		if ni.GetNetworkName() == networkName {
			return ni
		}
	}
	return &util.DefaultNetInfo{}
}

func (fnm *FakeNetworkManager) GetActiveNetwork(networkName string) util.NetInfo {
	return fnm.GetNetwork(networkName)
}

func (fnm *FakeNetworkManager) GetNetInfoForNADKey(nadKey string) util.NetInfo {
	fnm.Lock()
	defer fnm.Unlock()
	for _, ni := range fnm.PrimaryNetworks {
		for _, n := range ni.GetNADs() {
			if n == nadKey {
				return ni
			}
		}
	}
	return nil
}

func (fnm *FakeNetworkManager) GetActiveNetworkNamespaces(networkName string) ([]string, error) {
	namespaces := make([]string, 0)
	for namespaceName, primaryNAD := range fnm.PrimaryNetworks {
		nadNetworkName := primaryNAD.GetNADs()[0]
		if nadNetworkName != networkName {
			continue
		}
		namespaces = append(namespaces, namespaceName)
	}
	return namespaces, nil
}

func (fnm *FakeNetworkManager) DoWithLock(f func(network util.NetInfo) error) error {
	var errs []error
	for _, ni := range fnm.PrimaryNetworks {
		if err := f(ni); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (fnm *FakeNetworkManager) GetNetworkByID(id int) util.NetInfo {
	fnm.Lock()
	defer fnm.Unlock()
	for _, ni := range fnm.PrimaryNetworks {
		if ni.GetNetworkID() == id {
			return ni
		}
	}
	return nil
}
