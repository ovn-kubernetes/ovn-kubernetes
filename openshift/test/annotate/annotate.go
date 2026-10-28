// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package annotate

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"

	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/ginkgo/v2/types"
	"k8s.io/apimachinery/pkg/util/sets"
)

// Run generates tests annotations for the targeted package.
// It accepts two maps which defines labeling rules and filter
// function to remove elements based on test name and their labels.
// labelToTestName contains key as a label and values a list of strings which can be a partial, full or regex
// match of a test name. labelToLabel is used to map labels to labels - i.e. you can select tests by labels (ginkgo labels) and add labels.
func Run(labelToTestName, labelToLabel map[string][]string, filter func(name string) bool) {
	if len(os.Args) != 2 {
		fmt.Fprintf(os.Stderr, "error: requires exactly one argument which is a path to the output file")
		os.Exit(1)
	}
	// append labels from test map to test name
	if err := ginkgo.GetSuite().BuildTree(); err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to build ginkgo suite test tree: %v", err)
		os.Exit(1)
	}
	generator := newGenerator(labelToTestName, labelToLabel)
	ginkgo.GetSuite().WalkTests(generator.generate)
	if len(generator.errors) > 0 {
		for _, s := range generator.errors {
			fmt.Fprintf(os.Stderr, "appending label failed: %s\n", s)
		}
		os.Exit(1)
	}
	ginkgo.GetSuite().WalkTests(generator.appendLabelsToTestName)
	if len(generator.missing) > 0 {
		var names []string
		for name := range generator.missing {
			names = append(names, name)
		}
		sort.Strings(names)
		fmt.Fprintf(os.Stderr, "appending label failed:\n%s\n", strings.Join(names, "\n"))
		os.Exit(1)
	}

	var pairs []string
	for testName, labels := range generator.output {
		if filter(fmt.Sprintf("%s%s", testName, labels)) {
			continue
		}
		pairs = append(pairs, fmt.Sprintf("%q:\n%q,", testName, labels))
	}
	sort.Strings(pairs)
	contents := fmt.Sprintf(`// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package generated

import (
	"fmt"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/ginkgo/v2/types"
)

var AppendedAnnotations = map[string]string{
%s
}

func init() {
	ginkgo.GetSuite().SetAnnotateFn(func(name string, node types.TestSpec) {
		if newLabels, ok := AppendedAnnotations[name]; ok {
			node.AppendText(newLabels)
		} else {
			panic(fmt.Sprintf("unable to find test %%s", name))
		}
	})
}
`, strings.Join(pairs, "\n\n")) // double space between to ease readability
	generatedAnnotationsFileName := os.Args[len(os.Args)-1]
	if err := os.WriteFile(generatedAnnotationsFileName, []byte(contents), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to write generated annotations file at path %q: %v", generatedAnnotationsFileName, err)
		os.Exit(1)
	}
	if _, err := exec.Command("gofmt", "-s", "-w", generatedAnnotationsFileName).Output(); err != nil {
		fmt.Fprintf(os.Stderr, "error: failed to format golang file at path %q: %v", generatedAnnotationsFileName, err)
		os.Exit(1)
	}
}

// labelAppender is used to select tests and append labels to a ginkgo test name. It can select tests based on labels or regex or
// substring match. Prepended labels to a test name is not supported because ginkgo API does not allow it.
type labelAppender struct {
	// all labels we care about applying rules to and defined in openshift/test/pkg/annotate/rules.go derived from LabelToTestNameMatchMaps & LabelToTestNameMatchMaps
	labels []string
	// substrings match to apply a particular label derived from LabelToTestNameMatchMaps map
	stringMatches map[string][]string
	// regular expressions match to apply a particular label derived from LabelToTestNameMatchMaps map
	matches map[string]*regexp.Regexp
	// regular expressions match
	// see ExcludedTests in openshift/test/pkg/annotate/rules.go
	excludedTestsFilter *regexp.Regexp
	// key is label which maps to a list of labels defined in openshift/test/pkg/annotate/rules.go LabelToLabelMaps map
	labelToLabel map[string][]string

	// output from the generator and also input for appendLabelsToTestName
	output map[string]string
	// map of unmatched test names
	missing map[string]struct{}
	// a list of errors to display
	errors []string
}

func newGenerator(labelToTestNames, labelToLabels map[string][]string) *labelAppender {
	allLabelsSet := sets.New[string]()
	matches := make(map[string]*regexp.Regexp)
	stringMatches := make(map[string][]string)
	for label := range labelToLabels {
		allLabelsSet.Insert(label)
	}

	for label, matchTestNames := range labelToTestNames {
		sort.Strings(matchTestNames)
		allLabelsSet.Insert(label)
		var remain []string
		for _, matchTestName := range matchTestNames {
			// matchTestName maybe regex expression or partial test name or full test name
			re := regexp.MustCompile(matchTestName)
			if p, ok := re.LiteralPrefix(); ok {
				stringMatches[label] = append(stringMatches[label], p)
			} else {
				remain = append(remain, matchTestName)
			}
		}
		if len(remain) > 0 {
			matches[label] = regexp.MustCompile(strings.Join(remain, `|`))
		}
	}

	excludedTestsFilter := regexp.MustCompile(strings.Join(ExcludedTests, `|`))

	return &labelAppender{
		labels:              sets.List(allLabelsSet),
		stringMatches:       stringMatches,
		matches:             matches,
		labelToLabel:        labelToLabels,
		excludedTestsFilter: excludedTestsFilter,
		output:              make(map[string]string),
	}
}

func (r *labelAppender) appendLabelsToTestName(name string, node types.TestSpec) {
	if newLabels, ok := r.output[name]; ok {
		node.AppendText(newLabels)
	} else {
		r.missing[name] = struct{}{}
	}
}

func (r *labelAppender) generate(name string, node types.TestSpec) {
	newLabels := ""
	newName := name

	for {
		count := 0
		for _, label := range r.labels {
			// skip processing this label if it already exists in the test name
			if strings.Contains(newName, label) {
				continue
			}
			var isLabelRequired bool
			// check if there is a substring match from the test name
			for _, segment := range r.stringMatches[label] {
				isLabelRequired = strings.Contains(newName, segment)
				if isLabelRequired {
					break
				}
			}
			// check if there is a match from the test name using a regex
			if !isLabelRequired {
				if re := r.matches[label]; re != nil {
					isLabelRequired = r.matches[label].MatchString(newName)
				}
			}
			// check to see if the label is present - label is extracted from ginkgo labels
			if !isLabelRequired {
				if potentialLabels, ok := r.labelToLabel[label]; ok {
					for _, potentialLabel := range potentialLabels {
						isLabelRequired = containsLabel(node.Labels(), potentialLabel)
						if isLabelRequired {
							break
						}
					}
				}
			}

			if isLabelRequired {
				count++
				newLabels += label
				newName += label
			}
		}
		if count == 0 {
			break
		}
	}

	// Append suite name to test, if it doesn't already have one
	if !r.excludedTestsFilter.MatchString(newName) && !strings.Contains(newName, "[Suite:") {
		isSerial := strings.Contains(newName, "[Serial]")
		isConformance := strings.Contains(newName, "[Conformance]")
		switch {
		case isSerial && isConformance:
			newLabels += "[Suite:ovn-kubernetes/conformance/serial/minimal]"
		case isSerial:
			newLabels += "[Suite:ovn-kubernetes/conformance/serial]"
		case isConformance:
			newLabels += "[Suite:ovn-kubernetes/conformance/parallel/minimal]"
		default:
			newLabels += "[Suite:ovn-kubernetes/conformance/parallel]"
		}
	}

	if err := checkBalancedBrackets(newName); err != nil {
		r.errors = append(r.errors, err.Error())
	}

	r.output[name] = newLabels
}

// checkBalancedBrackets ensures that square brackets are balanced in generated test
// names. If they are not, it returns an error with the name of the test and a guess
// where the unmatched bracket(s) are.
func checkBalancedBrackets(testName string) error {
	stack := make([]int, 0, len(testName))
	for idx, c := range testName {
		switch c {
		case '[':
			stack = append(stack, idx)
		case ']':
			// case when we start off with a ]
			if len(stack) == 0 {
				stack = append(stack, idx)
			} else {
				stack = stack[:len(stack)-1]
			}
		}
	}

	if len(stack) > 0 {
		msg := testName + "\n"
	outerLoop:
		for i := 0; i < len(testName); i++ {
			for _, loc := range stack {
				if i == loc {
					msg += "^"
					continue outerLoop
				}
			}
			msg += " "
		}
		return fmt.Errorf("unbalanced brackets in test name:\n%s", msg)
	}

	return nil
}

// containLabel labels return true if labels are equal. Input args labels slice and/or candidate label may or may not contain brackets.
func containsLabel(labels []string, candidateLabel string) bool {
	if len(labels) == 0 {
		return false
	}
	for _, label := range labels {
		if stripBrackets(label) == stripBrackets(candidateLabel) {
			return true
		}
	}
	return false
}

func stripBrackets(s string) string {
	return strings.TrimFunc(s, func(r rune) bool {
		if r == '[' || r == ']' {
			return true
		}
		return false
	})
}
