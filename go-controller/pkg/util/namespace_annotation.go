// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package util

const (
	// Annotation used to enable/disable multicast in the namespace
	NsMulticastAnnotation = "k8s.ovn.org/multicast-enabled"
	// Annotation for enabling ACL logging to controller's log file
	AclLoggingAnnotation = "k8s.ovn.org/acl-logging"
)
