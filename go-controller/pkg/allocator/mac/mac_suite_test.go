package mac_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestMAC(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "MAC Suite")
}
