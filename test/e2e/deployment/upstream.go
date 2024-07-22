package deployment

type upstream struct {
}

func newUpstream() Deployment {
	return upstream{}
}

func (u upstream) OVNKubernetesNamespace() string {
	return "ovn-kubernetes"
}

func (u upstream) ExternalBridgeName() string {
	return "breth0"
}
