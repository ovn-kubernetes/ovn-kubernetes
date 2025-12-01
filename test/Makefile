E2E_REPORT_DIR:=$(CURDIR)/_artifacts
JOB_NAME?="$@"

# Make no assumptions about network, so run all tests. If on IPv4 or
# IPv6 only network, set appropriately to skip related tests.
PLATFORM_IPV4_SUPPORT?=false
PLATFORM_IPV6_SUPPORT?=false
DUALSTACK_CONVERSION?=false

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
	./scripts/e2e-kind.sh $@ $(WHAT); \
	result=$$?; \
	$(MAKE) check-coredumps; \
	coredump_result=$$?; \
	[ $$result -eq 0 ] && result=$$coredump_result; \
	exit $$result

.PHONY: control-plane
control-plane:
	E2E_REPORT_DIR=$(E2E_REPORT_DIR) \
	E2E_REPORT_PREFIX=$(JOB_NAME)_ \
	PLATFORM_IPV4_SUPPORT=$(PLATFORM_IPV4_SUPPORT) \
	PLATFORM_IPV6_SUPPORT=$(PLATFORM_IPV6_SUPPORT) \
	OVN_HA=$(OVN_HA) \
	./scripts/e2e-cp.sh $(WHAT); \
	result=$$?; \
	$(MAKE) check-coredumps; \
	coredump_result=$$?; \
	[ $$result -eq 0 ] && result=$$coredump_result; \
	exit $$result

.PHONY: conformance
conformance:
	E2E_REPORT_DIR=$(E2E_REPORT_DIR) \
	E2E_REPORT_PREFIX=$(JOB_NAME)_ \
	./scripts/conformance.sh; \
	result=$$?; \
	$(MAKE) check-coredumps; \
	coredump_result=$$?; \
	[ $$result -eq 0 ] && result=$$coredump_result; \
	exit $$result

.PHONY: tools
tools:
	./scripts/test-ovnkube-trace.sh

.PHONY: traffic-flow-tests
traffic-flow-tests:
	TRAFFIC_FLOW_TESTS=$(TRAFFIC_FLOW_TESTS) \
	./scripts/traffic-flow-tests.sh $(WHAT)

.PHONY: check-coredumps
check-coredumps:
	@exit_code=0; \
	KIND_CLUSTER_NAME=$${KIND_CLUSTER_NAME:-ovn}; \
	OCI_BIN=$${KIND_EXPERIMENTAL_PROVIDER:-docker}; \
	for node in $$(kind get nodes --name $$KIND_CLUSTER_NAME 2>/dev/null || echo ""); do \
		if $$OCI_BIN exec "$$node" sh -c 'ls /tmp/cores/core.* 2>/dev/null'; then \
			echo "ERROR: Core dump(s) found in node $$node!"; \
			$$OCI_BIN exec "$$node" sh -c 'ls -lh /tmp/cores/core.*'; \
			exit_code=1; \
		fi; \
	done; \
	if [ $$exit_code -eq 1 ]; then \
		echo "ERROR: core dumps found in KIND nodes"; \
		exit 1; \
	fi; \
