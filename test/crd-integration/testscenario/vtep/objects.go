// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package vtep

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	vtepv1 "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/crd/vtep/v1"
)

// newVTEP wraps a VTEP configuration with its resource metadata.
func newVTEP(name string, spec vtepv1.VTEPSpec) *vtepv1.VTEP {
	return &vtepv1.VTEP{
		TypeMeta:   metav1.TypeMeta{APIVersion: vtepv1.SchemeGroupVersion.String(), Kind: "VTEP"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       spec,
	}
}
