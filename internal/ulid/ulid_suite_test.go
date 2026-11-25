package ulid_test

import (
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestULID(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "ulid")
}
