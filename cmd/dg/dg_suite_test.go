package main_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/onsi/gomega/gexec"
)

func TestDg(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "dg binary")
}

var _ = AfterSuite(func() {
	gexec.CleanupBuildArtifacts()
})
