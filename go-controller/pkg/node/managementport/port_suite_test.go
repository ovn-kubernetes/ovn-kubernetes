package managementport

import (
	"testing"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"

	nodenft "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/node/nftables"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
)

func TestAdder(t *testing.T) {
	gomega.RegisterFailHandler(ginkgo.Fail)
	util.SetFakeIPTablesHelpers()
	nodenft.SetFakeNFTablesHelper()
	ginkgo.RunSpecs(t, "Management Port Suite")
}
