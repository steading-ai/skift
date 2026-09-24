package skiftv1connect_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestSkiftV1Connect(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "generated connect client")
}
