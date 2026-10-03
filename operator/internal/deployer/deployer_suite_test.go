// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package deployer_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

const (
	testOperatorName       = "test-operator"
	testNamespace          = "test-namespace"
	testConfigName         = "test-config"
	deploymentListKind     = "DeploymentList"
	nadGroup               = "k8s.cni.cncf.io"
	ovnGroup               = "k8s.ovn.org"
	deploymentTemplatePath = "templates/deployment.yaml"
	deploymentFileName     = "deployment.yaml"
	configMapTemplatePath  = "templates/configmap.yaml"
	configMapFileName      = "configmap.yaml"
)

func TestDeployer(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Deployer Suite")
}
