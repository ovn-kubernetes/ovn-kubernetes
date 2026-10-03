// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

// Package-level defaults for the OVN-Kubernetes container image.
// These are intentionally `var`s (not `const`) so that the build can
// stamp them via -ldflags "-X .../defaults.OvnKubeImage<Field>=value".
// At local dev time they are empty strings; CR-level overrides still apply.
package defaults

var (
	OvnKubeImageRepository string
	OvnKubeImageTag        string
	OvnKubeImageDigest     string // sha256:... or empty
)
