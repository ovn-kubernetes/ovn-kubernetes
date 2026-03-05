package testscenario

// ValidateCRScenario represent test scenario where a manifest is applied and failed with the expected error.
// Name, if non-empty, is the resource name used by cleanup to verify the resource was deleted.
// It should be set for valid scenarios (which actually create resources) and left empty for invalid ones.
type ValidateCRScenario struct {
	Description string
	Name        string
	Manifest    string
	ExpectedErr string
}
