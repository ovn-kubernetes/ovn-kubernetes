package nooverlay

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestNoOverlayController(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Cluster Manager No-Overlay Controller Suite")
}
