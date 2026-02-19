package v1

func (s *UserDefinedNetworkSpec) GetTopology() NetworkTopology {
	return s.Topology
}

func (s *UserDefinedNetworkSpec) GetLayer3() *Layer3Config {
	return s.Layer3
}

func (s *UserDefinedNetworkSpec) GetLayer2() *Layer2Config {
	return s.Layer2
}

func (s *UserDefinedNetworkSpec) GetLocalnet() *LocalnetConfig {
	// localnet is not supported
	return nil
}

func (s *UserDefinedNetworkSpec) GetTransport() TransportOption {
	// UDN (namespace-scoped) does not support EVPN transport
	return ""
}

func (s *UserDefinedNetworkSpec) GetEVPN() *EVPNConfig {
	// UDN (namespace-scoped) does not support EVPN
	return nil
}

func (s *NetworkSpec) GetTopology() NetworkTopology {
	return s.Topology
}

func (s *NetworkSpec) GetLayer3() *Layer3Config {
	return s.Layer3
}

func (s *NetworkSpec) GetLayer2() *Layer2Config {
	return s.Layer2
}

func (s *NetworkSpec) GetLocalnet() *LocalnetConfig {
	return s.Localnet
}

func (s *NetworkSpec) GetTransport() TransportOption {
	return s.Transport
}

func (s *NetworkSpec) GetEVPN() *EVPNConfig {
	return s.EVPN
}
