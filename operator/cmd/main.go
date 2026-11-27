// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"

	"github.com/ovn-kubernetes/ovn-kubernetes/operator/internal/operator"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "render" {
		operator.RunRender(os.Args[2:])
		return
	}
	operator.Run()
}
