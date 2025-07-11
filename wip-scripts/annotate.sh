#!/usr/bin/env bash

set -euo pipefail

counter=0
for node in $(kubectl get nodes -o name); do
	counter=$((counter + 1))
	chassis=$(kubectl get "$node" -o jsonpath='{.metadata.annotations.k8s\.ovn\.org/node-chassis-id}')
	kubectl annotate --overwrite "$node" \
		k8s.ovn.org/trust-zones='{"'"$chassis"'": ["tz'"$counter"'"]}'
done

echo "Annotated $counter nodes with trust zones."
