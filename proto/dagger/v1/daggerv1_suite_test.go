package daggerv1

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestDaggerV1(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "dagger.v1 contract")
}
