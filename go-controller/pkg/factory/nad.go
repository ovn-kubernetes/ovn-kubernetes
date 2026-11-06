// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package factory

import (
	"encoding/json"
	"fmt"

	nadapi "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/apis/k8s.cni.cncf.io/v1"
	nadclient "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/clientset/versioned"
	nadinformerfactory "github.com/k8snetworkplumbingwg/network-attachment-definition-client/pkg/client/informers/externalversions"

	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

// RouteAdvertisementsNADIndex indexes NADs by their RouteAdvertisements references.
const RouteAdvertisementsNADIndex = "network-manager-route-advertisements"

func (wf *WatchFactory) initNADInformer(client nadclient.Interface) error {
	wf.nadFactory = nadinformerfactory.NewSharedInformerFactory(client, resyncInterval)
	informer := wf.NADInformer().Informer()
	if util.IsRouteAdvertisementsEnabled() {
		// Node and controller managers share this informer and start concurrently.
		// Register its index once, before either manager can start.
		if err := informer.AddIndexers(cache.Indexers{RouteAdvertisementsNADIndex: nadRouteAdvertisementsIndex}); err != nil {
			return fmt.Errorf("failed to index NAD route advertisements: %w", err)
		}
	}
	var err error
	wf.informers[NetworkAttachmentDefinitionType], err = newQueuedInformer(eventQueueSize,
		NetworkAttachmentDefinitionType, informer, wf.stopChan, minNumEventQueues)
	return err
}

// nadRouteAdvertisementsIndex indexes each NAD under the RA names recorded in
// its annotation. These annotations reflect selection resolved by the RA
// controller, so consumers do not need to evaluate RA network selectors again.
// The informer cache maintains the index as NADs change or are deleted.
func nadRouteAdvertisementsIndex(obj interface{}) ([]string, error) {
	nad := obj.(*nadapi.NetworkAttachmentDefinition)
	annotation := nad.Annotations[types.OvnRouteAdvertisementsKey]
	if annotation == "" {
		return nil, nil
	}
	var names []string
	if err := json.Unmarshal([]byte(annotation), &names); err != nil {
		// Invalid annotations must not break informer cache updates. Network
		// reconciliation reports the error and retries the network.
		klog.Errorf("Failed to index route advertisements for NAD %s/%s: %v", nad.Namespace, nad.Name, err)
		return nil, nil
	}
	return names, nil
}
