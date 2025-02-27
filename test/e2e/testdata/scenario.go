package testdata

// ValidateCRScenario represent test scenario where a manifest is applied and failed with the expected error
// Or a manifest is applied and then updated, update fail with the expected error.
type ValidateCRScenario struct {
	Description     string
	Manifest        string
	MutatedManifest string
	ExpectedErr     string
}
