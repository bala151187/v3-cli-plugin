package uihelpers_test

import (
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	"testing"
)

func TestUIHelpers(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "UIHelpers Suite")
}
