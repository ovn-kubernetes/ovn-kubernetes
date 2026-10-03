// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package validator_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const (
	testName            = "test"
	testDeploymentName  = "test-deployment"
	testDaemonSetName   = "test-daemonset"
	testStatefulSetName = "test-statefulset"
	testPodName         = "test-pod"
	testCustomName      = "test-custom"
	customResourceKind  = "CustomResource"
)

// Field names used to build unstructured workload objects.
const (
	fieldAPIVersion      = "apiVersion"
	fieldKind            = "kind"
	fieldMetadata        = "metadata"
	fieldName            = "name"
	fieldNamespace       = "namespace"
	fieldSpec            = "spec"
	fieldReplicas        = "replicas"
	fieldStatus          = "status"
	fieldReadyReplicas   = "readyReplicas"
	fieldUpdatedReplicas = "updatedReplicas"
)

func TestValidator(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Validator Suite")
}
