package label

// overrideMap is used to rewrite label key and/or values. For example, if you want to rewrite Feature to a downstream specific name,
// therefore youd add "Feature" as a key to the overrides map and value to be what you wish to rewrite it to.
var overrideMap = map[string]string{}
