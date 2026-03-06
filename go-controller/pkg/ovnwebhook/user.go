package ovnwebhook

import (
	"strings"

	authenticationv1 "k8s.io/api/authentication/v1"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/csrapprover"
)

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
// (system:ovn-node:<nodeName> or system:ovn-node-dpu:<nodeName>) that is allowed to modify that node's annotations.
func ovnkubeNodeIdentity(user authenticationv1.UserInfo) (string, bool) {
	if strings.HasPrefix(user.Username, csrapprover.NamePrefixDPU+":") {
		nodeName := strings.TrimPrefix(user.Username, csrapprover.NamePrefixDPU+":")
		return nodeName, true
	}
	if !strings.HasPrefix(user.Username, csrapprover.NamePrefix+":") {
		return "", false
	}
	nodeName := strings.TrimPrefix(user.Username, csrapprover.NamePrefix+":")
	return nodeName, true
}
