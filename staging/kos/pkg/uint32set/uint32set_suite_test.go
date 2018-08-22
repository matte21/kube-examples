package uint32set_test

import (
    . "github.com/onsi/ginkgo"
    . "github.com/onsi/gomega"
	"testing"
)

func TestUInt32Set(t *testing.T) {
    RegisterFailHandler(Fail)
    RunSpecs(t, "uint32set Suite")
}
