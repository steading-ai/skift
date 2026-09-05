package daggerv1connect_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestDaggerV1Connect(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "generated connect client")
}
