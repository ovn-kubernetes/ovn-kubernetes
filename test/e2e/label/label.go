package label

// Label is a wrapper for ginkgo label. We need a wrapper because we want to constrain inputs. If Key and Value are not
// empty, then it will be concatenated together seperated by ':'. If Key is not empty and Value is empty, then only the Key is used.
type Label struct {
	// Key is mandatory
	Key string
	// Value is optional
	Value string
}

func (l Label) String() string {
	addBrackets := func(s string) string {
		return "[" + s + "]"
	}
	if l.Value == "" {
		return addBrackets(l.Key)
	}
	return addBrackets(l.Key + ":" + l.Value)
}

func (l Label) GinkgoLabel() string {
	if l.Value == "" {
		return l.Key
	}
	return l.Key + ":" + l.Value
}

func NewComponent(name string) Label {
	return Label{Key: processOverrides(name)}
}

func NewFeature(name string) Label {
	return Label{
		Key:   processOverrides("Feature"),
		Value: processOverrides(name),
	}
}

func NewLabel(parts ...string) Label {
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
