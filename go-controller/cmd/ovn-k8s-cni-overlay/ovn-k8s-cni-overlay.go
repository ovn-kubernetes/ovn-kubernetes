package main

import (
	"os"

	"github.com/containernetworking/cni/pkg/skel"
	"github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/cni/pkg/version"
	bv "github.com/containernetworking/plugins/pkg/utils/buildversion"
	"github.com/urfave/cli/v2"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/cni"
)

func main() {
	c := cli.NewApp()
	c.Name = "ovn-k8s-cni-overlay"
	c.Usage = "a CNI plugin to set up or tear down a container's network with OVN"
	c.Version = "0.0.2"

	p := cni.NewCNIPlugin("")
	c.Action = func(_ *cli.Context) error {
		return skel.PluginMainFuncsWithError(skel.CNIFuncs{Add: p.CmdAdd, Del: p.CmdDel, Check: p.CmdCheck}, version.All, bv.BuildString("ovn-k8s-cni-overlay"))
	}

	if err := c.Run(os.Args); err != nil {
		// Print the error to stdout in conformance with the CNI spec
		e, ok := err.(*types.Error)
		if !ok {
			e = &types.Error{Code: 100, Msg: err.Error()}
		}
		e.Print()
	}
}
