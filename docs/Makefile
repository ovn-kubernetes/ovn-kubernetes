# SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
# SPDX-License-Identifier: Apache-2.0

# Docs versioning (mike). See developer-guide/documentation.md and hack/build-docs.sh.
#
# Examples (from the docs/ directory):
#   make build-docs
#   make build-docs PUSH=1
#   make build-docs RELEASES=release-1.1
#   make build-docs RELEASES=release-1.1,master PUSH=1
#
# Or from the repository root:
#   make -C docs build-docs PUSH=1

RELEASES ?= all
PUSH ?= 0

.PHONY: build-docs
build-docs:
	../hack/build-docs.sh $(if $(filter 1 true YES yes,$(PUSH)),--push) --releases "$(RELEASES)"
