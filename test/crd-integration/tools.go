//go:build tools

// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

// Package crdintegration holds blank-import anchors for tool dependencies.
// The "tools" build tag is never set during a normal build so nothing here
// ends up in production binaries.  The import keeps `go mod tidy` from
// removing setup-envtest from go.mod/go.sum.
package crdintegration

import _ "sigs.k8s.io/controller-runtime/tools/setup-envtest"
