package ginkgo_wrapper

import (
	"strings"

	"github.com/onsi/ginkgo/v2"

	"github.com/ovn-org/ovn-kubernetes/test/e2e/label"
)

const componentName = "ovn-kubernetes"

var (
	Describe             = okDescribe()
	DescribeTableSubTree = okDescribeTableSubtree()
)

// okDescribe returns a wrapper function for ginkgo.Describe which prepends
// the CNI name plus any additional  labels.
func okDescribe() func(...interface{}) bool {
	return func(args ...interface{}) bool {
		args = append([]interface{}{label.NewComponent(componentName)}, args...)
		return registerInSuite(ginkgo.Describe, args)
	}
}

func okDescribeTableSubtree() func(...interface{}) bool {
	return func(args ...interface{}) bool {
		args = append([]interface{}{label.NewComponent(componentName)}, args...)
		return registerInSuite(ginkgo.DescribeTableSubtree, args)
	}
}

// registerInSuite is the common implementation of all wrapper functions. It
// expects to be called through one intermediate wrapper.
func registerInSuite(ginkgoCall func(string, ...interface{}) bool, args []interface{}) bool {
	var ginkgoArgs []interface{}
	var offset ginkgo.Offset
	var texts []string

	for _, arg := range args {
		switch arg := arg.(type) {
		case label.Label:
			ginkgoArgs = append(ginkgoArgs, ginkgo.Label(arg.GinkgoLabel()))
			texts = append(texts, arg.String())
		case ginkgo.Offset:
			offset = arg
		case string:
			if arg == "" {
				panic("labels must not empty strings as separators are unnecessary and need to be removed")
			}
			texts = append(texts, arg)
		default:
			ginkgoArgs = append(ginkgoArgs, arg)
		}
	}
	offset += 2 // This function and its direct caller.

	// Enforce that text snippets to not start or end with spaces because
	// those lead to double spaces when concatenating below.
	for _, text := range texts {
		if strings.HasPrefix(text, " ") || strings.HasSuffix(text, " ") {
			panic("trailing or leading spaces are unnecessary and need to be removed")
		}
	}

	ginkgoArgs = append(ginkgoArgs, offset)
	// compact labels and ensure test description contain a space between descriptions.
	var finalText string
	for _, text := range texts {
		if strings.HasPrefix(text, "[") {
			finalText += text
		} else {
			finalText += " " + text
		}
	}
	return ginkgoCall(finalText, ginkgoArgs...)
}
