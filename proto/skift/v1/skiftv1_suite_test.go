package skiftv1

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestSkiftV1(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "skift.v1 contract")
}
