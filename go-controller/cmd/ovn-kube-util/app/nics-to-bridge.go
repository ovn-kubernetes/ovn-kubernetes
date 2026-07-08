// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"fmt"

	"github.com/urfave/cli/v2"

	kexec "k8s.io/utils/exec"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	utilerrors "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util/errors"
)

// NicsToBridgeCommand creates ovs bridge for provided nic interfaces.
var NicsToBridgeCommand = cli.Command{
	Name:  "nics-to-bridge",
	Usage: "Create ovs bridge for nic interfaces",
	Flags: []cli.Flag{},
	Action: func(context *cli.Context) error {
		args := context.Args()
		if args.Len() == 0 {
			return fmt.Errorf("please specify list of nic interfaces")
		}

		if err := util.SetSpecificExec(kexec.New(), "ovs-vsctl"); err != nil {
			return err
		}

		var errorList []error
		for _, nic := range args.Slice() {
			// For CLI utility, pass 0 as MTU to skip MTU configuration
			// MTU should be set by the main ovnkube process with proper config
			if _, err := util.NicToBridge(nic, 0); err != nil {
				errorList = append(errorList, err)
			}
		}

		return utilerrors.Join(errorList...)
	},
}

// BridgesToNicCommand removes a NIC interface from OVS bridge and deletes the bridge
var BridgesToNicCommand = cli.Command{
	Name:  "bridges-to-nic",
	Usage: "Delete ovs bridge and move IP/routes to underlying NIC",
	Flags: []cli.Flag{},
	Action: func(context *cli.Context) error {
		args := context.Args()
		if args.Len() == 0 {
			return fmt.Errorf("please specify list of bridges")
		}

		if err := util.SetSpecificExec(kexec.New(), "ovs-vsctl"); err != nil {
			return err
		}

		var errorList []error
		for _, bridge := range args.Slice() {
			if err := util.BridgeToNic(bridge); err != nil {
				errorList = append(errorList, err)
			}
		}

		return utilerrors.Join(errorList...)
	},
}
