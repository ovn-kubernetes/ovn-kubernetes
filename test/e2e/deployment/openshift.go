package deployment

type openshift struct {
}

func newOpenshift() Deployment {
	return openshift{}
}

func (o openshift) OVNKubernetesNamespace() string {
	return "openshift-ovn-kubernetes"
}

func (o openshift) ExternalBridgeName() string {
	return "br-ex"
}
