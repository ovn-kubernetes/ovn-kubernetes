#!/usr/bin/env bash
set -euo pipefail

viddy 'kubectl exec -n ovn-kubernetes $(kubectl get pod -n ovn-kubernetes -l name=ovnkube-control-plane --no-headers -o custom-columns=":metadata.name") -- ovn-sbctl --columns=name,transport_zones,other_config list Chassis| cut -c -80'
