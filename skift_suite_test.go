package skift_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestSkift(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "development environment")
}
