package skift_test

import (
	"os"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestSkift(t *testing.T) {
	if os.Getenv("SKIFT_MUTATING") != "" {
		t.Skip("SKIFT_MUTATING is set: a mutation run leaves out the development-environment specs, which run make lint in copies of the tree")
	}
	RegisterFailHandler(Fail)
	RunSpecs(t, "development environment")
}
