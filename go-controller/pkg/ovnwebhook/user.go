package ovnwebhook

import (
	"strings"

	authenticationv1 "k8s.io/api/authentication/v1"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/csrapprover"
)

var ovnkubeClusterManagerServiceAccount = "system:serviceaccount:ovn-kubernetes:ovnkube-cluster-manager"

// checkNodeIdentity retrieves user name from UserInfo, based on given podAdmissions.
func checkNodeIdentity(podAdmissions []PodAdmissionConditionOption, user authenticationv1.UserInfo) (bool, *PodAdmissionConditionOption, string) {
	// check ovn prefix
	if strings.HasPrefix(user.Username, csrapprover.NamePrefix) {
		return true, nil, strings.TrimPrefix(user.Username, csrapprover.NamePrefix+":")
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

func ovnkubeNodeIdentity(user authenticationv1.UserInfo) (string, bool) {
	if !strings.HasPrefix(user.Username, csrapprover.NamePrefix) {
		return "", false
	}

	// Trim prefix and the last colon
	nodeName := strings.TrimPrefix(user.Username, csrapprover.NamePrefix+":")
	return nodeName, true
}

func isOVNKubeClusterManager(user authenticationv1.UserInfo) bool {
	return user.Username == ovnkubeClusterManagerServiceAccount
}
