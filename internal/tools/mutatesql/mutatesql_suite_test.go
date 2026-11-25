package main

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestMutateSQL(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "mutatesql")
}
