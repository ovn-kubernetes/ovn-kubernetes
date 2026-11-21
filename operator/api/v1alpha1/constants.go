// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

// OperatorNamespace is the only namespace where the OVNK8sOperator CR is
// reconciled. The manager's cache is restricted to this namespace, and the
// operator's RBAC grants access to ovnk8soperators only here.
const OperatorNamespace = "ovnk8s-operator-system"

// SingletonName is the required metadata.name of the OVNK8sOperator CR.
// The reconciler ignores CRs with any other name.
const SingletonName = "default"

// Deployment-mode values for GlobalConfig.DeploymentMode.
const (
	DeploymentModeStandard = "standard"
	DeploymentModeDpuHost  = "dpu-host"
	DeploymentModeDpu      = "dpu"
)

// Component resource names. Must match the resourceName entries in
// internal/defaults/config.yaml and the corresponding templates/<name>/
// directories.
const (
	ComponentOvnkubeControlPlane = "ovnkubeControlPlane"
	ComponentOvnkubeNode         = "ovnkubeNode"
	ComponentOvnkubeIdentity     = "ovnkubeIdentity"
	ComponentOvnkubeNodeDpuHost  = "ovnkubeNodeDpuHost"
	ComponentOvnkubeNodeDpu      = "ovnkubeNodeDpu"
)
