package networkmanager

import (
	"context"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/networkmanager"
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
	// namespace -> netInfo
	// if netInfo is nil, it represents a namespace which contains the required UDN label but with no valid network. It will return invalid network error.
	PrimaryNetworks map[string]util.NetInfo
}

func (fnm *FakeNetworkManager) Start() error { return nil }

func (fnm *FakeNetworkManager) Stop() {}

func (fnm *FakeNetworkManager) Interface() networkmanager.Interface {
	return fnm
}

func (fnm *FakeNetworkManager) GetActiveNetworkForNamespace(namespace string) (util.NetInfo, error) {
	network := fnm.GetActiveNetworkForNamespaceFast(namespace)
	if network == nil {
		return nil, util.NewInvalidPrimaryNetworkError(namespace)
	}
	return network, nil
}

func (fnm *FakeNetworkManager) GetActiveNetworkForNamespaceFast(namespace string) util.NetInfo {
	if primaryNetworks, ok := fnm.PrimaryNetworks[namespace]; ok {
		return primaryNetworks
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
