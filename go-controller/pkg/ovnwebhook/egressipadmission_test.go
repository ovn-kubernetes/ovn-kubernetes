package ovnwebhook

import (
	"context"
	"fmt"
	"testing"

	egressipv1 "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/crd/egressip/v1"
	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/util"
	v1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

var egressIPName = "newEgressIP"

func TestEgressIPAdmission_ValidateCreate(t *testing.T) {
	adm := NewEgressIPAdmissionWebhook("")
	tests := []struct {
		name        string
		ctx         context.Context
		newObj      runtime.Object
		expectedErr error
	}{
		{
			name: "disallow egressIP creation if egressip-mark annotation is present while EIP creation",
			ctx: admission.NewContextWithRequest(context.TODO(), admission.Request{
				AdmissionRequest: v1.AdmissionRequest{UserInfo: authenticationv1.UserInfo{
					Username: "system:serviceaccount:ovn-kubernetes:ovnkube-cluster-manager",
				}},
			}),
			newObj: &egressipv1.EgressIP{
				ObjectMeta: metav1.ObjectMeta{
					Name:        egressIPName,
					Labels:      map[string]string{"key": "new"},
					Annotations: map[string]string{util.EgressIPMarkAnnotation: "50000"},
				},
			},
			expectedErr: fmt.Errorf("annotation[s] %s is/are not allowed to be added while creating Egress IP", util.EgressIPMarkAnnotation),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := adm.ValidateCreate(tt.ctx, tt.newObj)
			if err == nil {
				t.Fatalf("ValidateCreate() expected error %v, got nil", tt.expectedErr)
				return
			}
			if err != tt.expectedErr && err.Error() != tt.expectedErr.Error() {
				t.Errorf("ValidateUpdate() error = %v, expectedErr %v", err, tt.expectedErr)
				return
			}
		})
	}
}
