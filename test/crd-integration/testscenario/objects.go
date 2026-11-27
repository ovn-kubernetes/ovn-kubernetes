// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package testscenario

import (
	"strings"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// WithField copies an object and sets a dot-separated field path, preserving
// explicit null or empty values that typed objects omit when serialized.
func WithField(object client.Object, value any, path string) *unstructured.Unstructured {
	obj := toUnstructured(object)
	if err := unstructured.SetNestedField(obj.Object, value, strings.Split(path, ".")...); err != nil {
		panic(err)
	}
	return obj
}

// WithoutFields copies an object and removes dot-separated field paths to test
// required fields that typed objects always serialize, even when empty.
func WithoutFields(object client.Object, paths ...string) *unstructured.Unstructured {
	obj := toUnstructured(object)
	for _, path := range paths {
		unstructured.RemoveNestedField(obj.Object, strings.Split(path, ".")...)
	}
	return obj
}

// toUnstructured copies a fixture without changing its serialized field values.
func toUnstructured(object client.Object) *unstructured.Unstructured {
	obj, err := runtime.DefaultUnstructuredConverter.ToUnstructured(object.DeepCopyObject())
	if err != nil {
		panic(err)
	}
	return &unstructured.Unstructured{Object: obj}
}
