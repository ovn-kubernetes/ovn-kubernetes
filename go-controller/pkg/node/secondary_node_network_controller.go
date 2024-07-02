package node

import (
	"context"
	"fmt"
	"sync"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/factory"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"

	"k8s.io/klog/v2"
)

// SecondaryNodeNetworkController structure is the object which holds the controls for starting
// and reacting upon the watched resources (e.g. pods, endpoints) for secondary network
type SecondaryNodeNetworkController struct {
	BaseNodeNetworkController
	// pod events factory handler
	podHandler *factory.Handler

	networkID int
}

// NewSecondaryNodeNetworkController creates a new OVN controller for creating logical network
// infrastructure and policy for default l3 network
func NewSecondaryNodeNetworkController(cnnci *CommonNodeNetworkControllerInfo, netInfo util.NetInfo) *SecondaryNodeNetworkController {
	return &SecondaryNodeNetworkController{
		BaseNodeNetworkController: BaseNodeNetworkController{
			CommonNodeNetworkControllerInfo: *cnnci,
			NetInfo:                         netInfo,
			stopChan:                        make(chan struct{}),
			wg:                              &sync.WaitGroup{},
		},
	}
}

// Start starts the default controller; handles all events and creates all needed logical entities
func (nc *SecondaryNodeNetworkController) Start(ctx context.Context) error {
	klog.Infof("Start secondary node network controller of network %s", nc.GetNetworkName())
	handler, err := nc.watchPodsDPU()
	if err != nil {
		return err
	}
	nc.podHandler = handler

	if err := nc.ensureNetworkID(); err != nil {
		return fmt.Errorf("failed ensuring network id at user defined node controller for network '%s': %w", nc.GetNetworkName(), err)
	}

	return nil
}

// Stop gracefully stops the controller
func (nc *SecondaryNodeNetworkController) Stop() {
	klog.Infof("Stop secondary node network controller of network %s", nc.GetNetworkName())
	close(nc.stopChan)
	nc.wg.Wait()

	if nc.podHandler != nil {
		nc.watchFactory.RemovePodHandler(nc.podHandler)
	}
}

// Cleanup cleans up node entities for the given secondary network
func (nc *SecondaryNodeNetworkController) Cleanup() error {
	return nil
}

// TODO(dceara): identical to BaseSecondaryNetworkController.ensureNetworkID()
func (nc *SecondaryNodeNetworkController) ensureNetworkID() error {
	if nc.networkID != 0 {
		return nil
	}
	nodes, err := nc.watchFactory.GetNodes()
	if err != nil {
		return fmt.Errorf("failed to get nodes: %v", err)
	}
	networkID := util.InvalidNetworkID
	for _, node := range nodes {
		networkID, err = util.ParseNetworkIDAnnotation(node, nc.GetNetworkName())
		if err != nil {
			//TODO Warning
			continue
		}
	}
	if networkID == util.InvalidNetworkID {
		return fmt.Errorf("missing network id for network '%s'", nc.GetNetworkName())
	}
	nc.networkID = networkID
	return nil
}
