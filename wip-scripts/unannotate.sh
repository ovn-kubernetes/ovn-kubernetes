#!/usr/bin/env bash

set -euo pipefail

for node in $(kubectl get nodes -o name); do
		kubectl annotate --overwrite "$node" \
			k8s.ovn.org/trust-zones-
done
