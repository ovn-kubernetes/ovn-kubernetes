// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package ovnwebhook

import (
	"strings"

	authenticationv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/csrapprover"
)

const (
	// OvnKubeNodeDPUServiceAccountName is the service account DPU ovnkube-node processes use
	// to access the DPU-host cluster.
	OvnKubeNodeDPUServiceAccountName = "ovnkube-node-dpu"
)

// isOvnKubeNodeDPUUser reports whether username is the ovnkube-node-dpu service account.
func isOvnKubeNodeDPUUser(username string) bool {
	const prefix = "system:serviceaccount:"
	if !strings.HasPrefix(username, prefix) {
		return false
	}
	namespaceAndSA := strings.TrimPrefix(username, prefix)
	namespace, saName, ok := strings.Cut(namespaceAndSA, ":")
	return ok && namespace != "" && saName == OvnKubeNodeDPUServiceAccountName
}

// isDPUExtraAllowedUser reports whether username is registered in extraAllowedUsers as the DPU SA.
func isDPUExtraAllowedUser(extraAllowedUsers sets.Set[string], username string) bool {
	return extraAllowedUsers.Has(username) && isOvnKubeNodeDPUUser(username)
}

// checkNodeIdentity retrieves user name from UserInfo, based on given podAdmissions.
func checkNodeIdentity(podAdmissions []PodAdmissionConditionOption, user authenticationv1.UserInfo) (bool, *PodAdmissionConditionOption, string) {
	if nodeName, ok := ovnkubeNodeIdentity(user); ok {
		return true, nil, nodeName
	}

	// check prefix in podAdmissions
	for i, v := range podAdmissions {
		if !strings.HasPrefix(user.Username, v.CommonNamePrefix) {
			continue
		}
		return false, &podAdmissions[i], strings.TrimPrefix(user.Username, v.CommonNamePrefix+":")
	}
	return false, nil, ""
}

// ovnkubeNodeIdentity returns the node name and true if the user is an ovnkube-node identity
// (system:ovn-node:<nodeName>) that is allowed to modify that node's annotations.
func ovnkubeNodeIdentity(user authenticationv1.UserInfo) (string, bool) {
	if !strings.HasPrefix(user.Username, csrapprover.NamePrefix+":") {
		return "", false
	}
	nodeName := strings.TrimPrefix(user.Username, csrapprover.NamePrefix+":")
	return nodeName, true
}

// dpuHostNodeIdentity returns the node name and true if the user is an ovnkube-node identity
// running in dpu-host mode (system:ovn-node-dpu-host:<nodeName>). dpu-host uses its own
// per-node common name prefix so it gets a restricted node-annotation allowlist.
func dpuHostNodeIdentity(user authenticationv1.UserInfo) (string, bool) {
	if !strings.HasPrefix(user.Username, csrapprover.NamePrefixDPUHost+":") {
		return "", false
	}
	nodeName := strings.TrimPrefix(user.Username, csrapprover.NamePrefixDPUHost+":")
	return nodeName, true
}
