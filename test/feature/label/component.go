// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package label

import "github.com/onsi/ginkgo/v2"

func ComponentName() ginkgo.Labels {
	return NewComponent("ovn-kubernetes")
}
