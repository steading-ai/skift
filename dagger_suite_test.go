package dagger_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestDagger(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "development environment")
}
