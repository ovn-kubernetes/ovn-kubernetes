package testscenario

// ValidateCRScenario represent test scenario where a manifest is applied and failed with the expected error
type ValidateCRScenario struct {
	Description string
	Manifest    string
	ExpectedErr string
}
