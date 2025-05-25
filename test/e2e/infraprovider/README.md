# Infra Provider
Infra Provider provides test dependencies using an infrastructure agnostic API.

## Motivation
Previous to this API, our tests relied directly on upstream "KinD" to provision networks and launch external hosts.
This prevented downstream consumption of upstream tests.

## Description
Providers external to the cluster resources including adding external hosts [1] and provisioning networks,
attaching networks, etc.

[1] deployed as containers on KinD provider but may be deployed as host-networked container downstream.

Known implementations:
- KinD
- OpenShift



