package networkmanager

import (
	"context"
	"sync"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/networkmanager"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/types"
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

func (fcm *FakeControllerManager) NewNetworkController(netInfo util.NetInfo) (networkmanager.NetworkController, error) {
	return &FakeNetworkController{netInfo}, nil
}

func (fcm *FakeControllerManager) CleanupStaleNetworks(_ ...util.NetInfo) error {
	return nil
}

func (fcm *FakeControllerManager) GetDefaultNetworkController() networkmanager.ReconcilableNetworkController {
	return nil
}

func (fcm *FakeControllerManager) Reconcile(_ string, _, _ util.NetInfo) error {
	return nil
}

type FakeNetworkManager struct {
	sync.Mutex
	// namespace -> netInfo
	PrimaryNetworks map[string]util.NetInfo
	// name -> netInfo
	OtherNetworks map[string]util.NetInfo
}

func (fnm *FakeNetworkManager) Start() error { return nil }

func (fnm *FakeNetworkManager) Stop() {}

func (fnm *FakeNetworkManager) GetActiveNetworkForNamespace(namespace string) (util.NetInfo, error) {
	return fnm.GetActiveNetworkForNamespaceFast(namespace), nil
}

func (fnm *FakeNetworkManager) GetActiveNetworkForNamespaceFast(namespace string) util.NetInfo {
	fnm.Lock()
	defer fnm.Unlock()
	if fnm.PrimaryNetworks[namespace] != nil {
		return fnm.PrimaryNetworks[namespace]
	}
	network := fnm.getNetwork(types.DefaultNetworkName)
	if network == nil {
		return &util.DefaultNetInfo{}
	}
	return network
}

func (fnm *FakeNetworkManager) GetNetwork(networkName string) util.NetInfo {
	fnm.Lock()
	defer fnm.Unlock()
	return fnm.getNetwork(networkName)
}

func (fnm *FakeNetworkManager) getNetwork(networkName string) util.NetInfo {
	for _, ni := range fnm.PrimaryNetworks {
		if ni.GetNetworkName() == networkName {
			return ni
		}
	}
	return fnm.OtherNetworks[networkName]
}

func (fnm *FakeNetworkManager) GetActiveNetworkNamespaces(networkName string) ([]string, error) {
	fnm.Lock()
	defer fnm.Unlock()
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
	fnm.Lock()
	defer fnm.Unlock()
	var errs []error
	for _, ni := range fnm.PrimaryNetworks {
		if err := f(ni); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
