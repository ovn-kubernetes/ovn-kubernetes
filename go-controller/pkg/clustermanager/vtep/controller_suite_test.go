package vtep

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestVTEPController(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "VTEP Controller Suite")
}
