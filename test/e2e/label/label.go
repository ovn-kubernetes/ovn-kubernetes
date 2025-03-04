package label

import "github.com/onsi/ginkgo/v2"

// Label is a wrapper for ginkgo label. We need a wrapper because we want to constrain inputs. If Key and Value are not
// empty, then it will be concatenated together seperated by ':'. If Key is not empty and Value is empty, then only the Key is used.
type Label struct {
	// Key is mandatory
	Key string
	// Value is optional
	Value string
}

func (l Label) GinkgoLabel() ginkgo.Labels {
	if l.Value == "" {
		return ginkgo.Label(l.Key)
	}
	return ginkgo.Label(l.Key + ":" + l.Value)
}

func NewComponent(name string) ginkgo.Labels {
	return New(name, "").GinkgoLabel()
}

func New(parts ...string) Label {
	if len(parts) == 0 || len(parts) > 2 {
		panic("invalid number of label constituents")
	}
	key, val := processOverrides(parts[0]), processOverrides(parts[1])
	return Label{
		Key:   key,
		Value: val,
	}
}

func processOverrides(s string) string {
	overRide, ok := overrideMap[s]
	if !ok {
		return s
	}
	return overRide
}
