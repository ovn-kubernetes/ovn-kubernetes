package testdata

// ValidateCRScenario represent test scenario where a manifest is applied and failed with the expected error
type ValidateCRScenario struct {
	Manifest    string
	ExpectedErr string
}
