E2E_REPORT_DIR:=$(CURDIR)/_artifacts
JOB_NAME?="$@"

# Make no assumptions about network, so run all tests. If on IPv4 or
# IPv6 only network, set appropriately to skip related tests.
PLATFORM_IPV4_SUPPORT?=false
PLATFORM_IPV6_SUPPORT?=false
DUALSTACK_CONVERSION?=false

# Coredump detection settings
# Directory where kind clusters store coredumps
COREDUMP_DIR?=/tmp/kind/logs/coredumps
# Processes to skip when checking for coredumps (pipe-separated for grep)
# https://github.com/ovn-kubernetes/ovn-kubernetes/issues/5782
SKIPPED_COREDUMPS?=zebra|bgpd|mgmtd|bfdd

# Check for coredumps and fail if any are found (excluding skipped processes)
# Usage: $(call check-coredumps)
define check-coredumps
	@if [ -d "$(COREDUMP_DIR)" ]; then \
		coredumps=$$(find "$(COREDUMP_DIR)" -maxdepth 1 -type f 2>/dev/null | grep -v -E '$(SKIPPED_COREDUMPS)' || true); \
		if [ -n "$$coredumps" ]; then \
			echo "ERROR: Coredumps found:"; \
			echo "$$coredumps"; \
			exit 1; \
		fi; \
	fi
endef

.PHONY: install-kind
install-kind:
	PLATFORM_IPV4_SUPPORT=$(PLATFORM_IPV4_SUPPORT) \
	PLATFORM_IPV6_SUPPORT=$(PLATFORM_IPV6_SUPPORT) \
	./scripts/install-kind.sh

.PHONY : upgrade-ovn
upgrade-ovn:
	./scripts/upgrade-ovn.sh

.PHONY: shard-%
shard-%:
	E2E_REPORT_DIR=$(E2E_REPORT_DIR) \
	E2E_REPORT_PREFIX=$(JOB_NAME)_ \
	PLATFORM_IPV4_SUPPORT=$(PLATFORM_IPV4_SUPPORT) \
	PLATFORM_IPV6_SUPPORT=$(PLATFORM_IPV6_SUPPORT) \
	DUALSTACK_CONVERSION=$(DUALSTACK_CONVERSION) \
	SINGLE_NODE_CLUSTER=$(SINGLE_NODE_CLUSTER) \
	./scripts/e2e-kind.sh $@ $(WHAT)
	$(call check-coredumps)

# control-plane: coredump check is handled by the test framework (wrappedTestFramework)
.PHONY: control-plane
control-plane:
	E2E_REPORT_DIR=$(E2E_REPORT_DIR) \
	E2E_REPORT_PREFIX=$(JOB_NAME)_ \
	PLATFORM_IPV4_SUPPORT=$(PLATFORM_IPV4_SUPPORT) \
	PLATFORM_IPV6_SUPPORT=$(PLATFORM_IPV6_SUPPORT) \
	OVN_HA=$(OVN_HA) \
	./scripts/e2e-cp.sh $(WHAT)

.PHONY: conformance
conformance:
	E2E_REPORT_DIR=$(E2E_REPORT_DIR) \
	E2E_REPORT_PREFIX=$(JOB_NAME)_ \
	./scripts/conformance.sh
	$(call check-coredumps)

.PHONY: tools
tools:
	./scripts/test-ovnkube-trace.sh
	$(call check-coredumps)

.PHONY: traffic-flow-tests
traffic-flow-tests:
	TRAFFIC_FLOW_TESTS=$(TRAFFIC_FLOW_TESTS) \
	./scripts/traffic-flow-tests.sh $(WHAT)
	$(call check-coredumps)
