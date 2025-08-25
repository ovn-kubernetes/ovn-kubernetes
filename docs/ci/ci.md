# CI Tests

For CI, OVN-Kubernetes runs the
[Kubernetes E2E tests](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-testing/e2e-tests.md)
and some [locally defined](https://github.com/ovn-org/ovn-kubernetes/tree/master/test/e2e) tests. 
[GitHub Actions](https://help.github.com/en/actions)
are used to run a subset of the Kubernetes E2E tests on each pull request. The
local workflow that controls the test run is located in
[ovn-kubernetes/.github/workflows/test.yml](https://github.com/ovn-org/ovn-kubernetes/blob/master/.github/workflows/test.yml).

The following tasks are performed:

- Build OVN-Kubernetes
- Check out the Kubernetes source tree and compiles some dependencies
- Install KIND
- Run a matrix of End-To-End Tests using KIND
- Ensure that documentation builds successfully

The full matrix of e2e tests found [here](https://github.com/ovn-org/ovn-kubernetes/blob/master/.github/workflows/test.yml)
are also run periodically (twice daily) using an OVN-Kubernetes build based on the currently merged code base.

The following sections should help you understand (and if needed modify) the set of tests that run and how to run these
tests locally.

## Understanding the CI Test Suite

The tests are broken into 2 categories, `shard` tests which execute tests from the Kubernetes E2E test suite and the
`control-plane` tests which run locally defined tests.

### Shard tests

The shard tests are broken into a set of shards, which is just a grouping of tests,
and each shard is run in a separate job in parallel. Shards execute the `shard-%` target in 
[ovn-kubernetes/test/Makefile](https://github.com/ovn-org/ovn-kubernetes/blob/master/test/Makefile).
The set of shards may change in the future. Below is an example of the shards at time of this writing:

- shard-network
  - All E2E tests that match `[sig-network]`
- shard-conformance
  - All E2E tests that match `[Conformance]|[sig-network]`
- shard-test
  - Single E2E test that matches the name of the test specified with a regex. 
  - When selecting the `shard-test` target, you focus on a specific test by appending `WHAT=<test name>` to the make command.
  - See bottom of this document for an example.

Shards use the [E2E framework](https://kubernetes.io/blog/2019/03/22/kubernetes-end-to-end-testing-for-everyone/). By
selecting a specific shard, you modify ginkgo's `--focus` parameter.

The regex expression for determining which E2E test is run in which shard, as
well as the list of skipped tests is defined in
[ovn-kubernetes/test/scripts/e2e-kind.sh](https://github.com/ovn-org/ovn-kubernetes/blob/master/test/scripts/e2e-kind.sh).

### Control-plane tests

In addition to the `shard-%` tests, there is also a `control-plane` target in 
[ovn-kubernetes/test/Makefile](https://github.com/ovn-org/ovn-kubernetes/blob/master/test/Makefile).
Below is a description of this target:

- control-plane
  - All locally defined tests by default.
  - You can focus on a specific test by appending `WHAT=<test name>` to the make command.
  - See bottom of this document for an example.

All local tests are run by `make control-plane`. The local tests are controlled in
[ovn-kubernetes/test/scripts/e2e-cp.sh](https://github.com/ovn-org/ovn-kubernetes/blob/master/test/scripts/e2e-cp.sh)
and the actual tests are defined in the directory
[ovn-kubernetes/test/e2e/](https://github.com/ovn-org/ovn-kubernetes/tree/master/test/e2e).

#### Node IP migration tests

The node IP migration tests are part of the control-plane tests but due to their impact they cannot be run concurrently
with other tests and they are disabled when running `make control-plane`.
Instead, they must explicitly be requested with `make -C test control-plane WHAT="Node IP address migration"`.

### Github CI integration through Github Actions Matrix

Each of these shards and control-plane tests can then be run in a [Github Actions matrix](https://docs.github.com/en/actions/learn-github-actions/managing-complex-workflows#using-a-build-matrix) of:
* HA setup (3 masters and 0 workers) and a non-HA setup (1 master and 2 workers)
* Local Gateway Mode and Shared Gateway Mode. See:
[Enable Node-Local Services Access in Shared Gateway Mode](https://github.com/ovn-org/ovn-kubernetes/blob/master/docs/design/shared_gw_dgp.md)
* IPv4 Only, IPv6 Only and Dualstack
* Disabled SNAT Multiple Gateways or Enabled SNAT Gateways
* Single bridge or two bridges

To reduce the explosion of tests being run in CI, the test cases run are limited
using an `exclude:` statement in 
[ovn-kubernetes/.github/workflows/test.yml](https://github.com/ovn-org/ovn-kubernetes/blob/master/.github/workflows/test.yml).

# Conformance Tests

We have a conformance test suit that can be invoked using the `make conformance` command.
Currently we run the `TestNetworkPolicyV2Conformance` tests there. The actual tests are
defined in https://github.com/kubernetes-sigs/network-policy-api/tree/master/conformance
and then invoked from this repo. Any changes to the tests first have to be submitted
upstream to `network-policy-api` repo and then brought downstream into the ovn-kubernetes repo
through version bump.

# Documentation Build Check

To catch any potential documentation build breakages which would prevent any docs changes
from being deployed to our GitHub Pages [site](https://github.com/ovn-org/ovn-kubernetes). The build check will produce the
html docs and will be available in the job artifacts for review. There is a link printed
in the job run logs inside the step "Upload Artifact". Download and unzip that locally 
to view the resulting docs after they are built to see what would be deployed to github
pages.