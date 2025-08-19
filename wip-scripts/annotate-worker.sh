#!/usr/bin/env bash

set -euo pipefail

for node in $(kubectl get nodes -o name | grep ovn-worker2); do
	chassis=$(kubectl get "$node" -o jsonpath='{.metadata.annotations.k8s\.ovn\.org/node-chassis-id}')
	kubectl annotate --overwrite "$node" \
		k8s.ovn.org/trust-zones='{"'"$chassis"'": ["tz-worker"]}'
done
