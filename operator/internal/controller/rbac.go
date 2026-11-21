// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package controller

// Cluster-scoped resource management and the union of active component ClusterRole rules.
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces/status,verbs=get;patch;update
// +kubebuilder:rbac:groups="",resources=nodes;nodes/status,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups="",resources=pods/status,verbs=get;patch;update
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services/status,verbs=patch;update
// +kubebuilder:rbac:groups="",resources=endpoints,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch;update
// +kubebuilder:rbac:groups=admissionregistration.k8s.io,resources=validatingadmissionpolicies;validatingadmissionpolicybindings;validatingwebhookconfigurations,verbs=get;list;create;patch;delete
// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,verbs=get;list;create;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles;clusterrolebindings,verbs=get;list;create;patch;delete
// +kubebuilder:rbac:groups=certificates.k8s.io,resources=certificatesigningrequests,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=certificates.k8s.io,resources=certificatesigningrequests/approval,verbs=update
// +kubebuilder:rbac:groups=certificates.k8s.io,resources=signers,resourceNames=kubernetes.io/kube-apiserver-client,verbs=approve
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch;create;update;delete;deletecollection
// +kubebuilder:rbac:groups=k8s.cni.cncf.io,resources=ipamclaims;multi-networkpolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups=k8s.cni.cncf.io,resources=ipamclaims/status,verbs=patch;update
// +kubebuilder:rbac:groups=k8s.cni.cncf.io,resources=network-attachment-definitions,verbs=get;list;watch;create;patch;update;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups=policy.networking.k8s.io,resources=adminnetworkpolicies;baselineadminnetworkpolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups=policy.networking.k8s.io,resources=adminnetworkpolicies/status;baselineadminnetworkpolicies/status,verbs=patch;update
// +kubebuilder:rbac:groups=k8s.ovn.org,resources=adminpolicybasedexternalroutes;adminpolicybasedroutes;clusternetworkconnects;clusteruserdefinednetworks;egressfirewalls;egressips;egressqoses;egressservices;ipreservations;networkqoses;routeadvertisements;uplinks;uplinkstates;userdefinednetworks;vteps,verbs=get;list;watch
// +kubebuilder:rbac:groups=k8s.ovn.org,resources=adminpolicybasedexternalroutes/status;clusternetworkconnects;clusternetworkconnects/status;clusteruserdefinednetworks;clusteruserdefinednetworks/finalizers;clusteruserdefinednetworks/status;egressfirewalls/status;egressips;egressqoses/status;egressservices/status;ipreservations/status;networkqoses;networkqoses/status;routeadvertisements;routeadvertisements/status;uplinks;uplinks/status;uplinkstates;userdefinednetworks;userdefinednetworks/status;vteps;vteps/status,verbs=patch;update
// +kubebuilder:rbac:groups=k8s.ovn.org,resources=routeadvertisements;uplinkstates,verbs=create;delete
// +kubebuilder:rbac:groups=k8s.ovn.org,resources=adminpolicybasedroutes/status,verbs=patch;update
// +kubebuilder:rbac:groups=frrk8s.metallb.io,resources=frrconfigurations,verbs=get;list;watch;create;patch;update;delete
// +kubebuilder:rbac:groups=network.openshift.io,resources=dnsnameresolvers,verbs=get;list;watch;create;patch;update;delete

// Target-namespace deployment and the union of active component Role rules.
// +kubebuilder:rbac:groups="",namespace=ovn-kubernetes,resources=serviceaccounts;services,verbs=get;list;create;patch;delete
// +kubebuilder:rbac:groups="",namespace=ovn-kubernetes,resources=secrets,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups="",namespace=ovn-kubernetes,resources=configmaps,verbs=get;list;watch;create;patch;update;delete
// +kubebuilder:rbac:groups="",namespace=ovn-kubernetes,resources=endpoints,verbs=get;create
// +kubebuilder:rbac:groups=apps,namespace=ovn-kubernetes,resources=deployments;daemonsets,verbs=get;list;create;patch;delete
// +kubebuilder:rbac:groups=policy,namespace=ovn-kubernetes,resources=poddisruptionbudgets,verbs=get;list;create;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,namespace=ovn-kubernetes,resources=roles;rolebindings,verbs=get;list;create;patch;delete
// +kubebuilder:rbac:groups=coordination.k8s.io,namespace=ovn-kubernetes,resources=leases,verbs=get;list;create;update

// Operator-namespace image-pull Secret access and optional test-hook execution.
// +kubebuilder:rbac:groups="",namespace=ovnk8s-operator-system,resources=secrets,verbs=get
// +kubebuilder:rbac:groups="",namespace=ovnk8s-operator-system,resources=serviceaccounts,verbs=create;delete
// +kubebuilder:rbac:groups="",namespace=ovnk8s-operator-system,resources=pods,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",namespace=ovnk8s-operator-system,resources=pods/exec,verbs=create
// +kubebuilder:rbac:groups="",namespace=ovnk8s-operator-system,resources=services,verbs=get;list
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,namespace=ovnk8s-operator-system,resources=roles;rolebindings,verbs=create;delete
// +kubebuilder:rbac:groups=batch,namespace=ovnk8s-operator-system,resources=jobs,verbs=get;create;delete
