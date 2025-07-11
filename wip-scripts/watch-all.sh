#!/usr/bin/env bash
set -euo pipefail

viddy 'for nodepod in $(kubectl get pod -n ovn-kubernetes -l name=ovnkube-node --no-headers -o custom-columns=":metadata.name"); do echo; echo -e "\e[32m$nodepod:\e[0m"; kubectl exec -n ovn-kubernetes $nodepod -c ovn-controller -- ovn-sbctl --columns=name,transport_zones,other_config list Chassis | cut -c -80; done'

