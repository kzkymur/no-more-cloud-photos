package job

import (
	"testing"

	profiledefinition "github.com/kzkymur/no-more-cloud-photos/internal/profile"
)

func TestValidatePinnedOutputAllowsStaticWebPAVIF(t *testing.T) {
	definition := profiledefinition.StandardV1()
	pinned := Profile{ID: definition.ID, Key: definition.Key, Version: definition.Version, Processor: definition.Processor,
		ParametersSchemaVersion: definition.ParametersSchemaVersion, InputMIMETypes: definition.InputMIMETypes, Parameters: definition.Parameters}
	if err := validatePinnedOutput(pinned, "active", "image/webp", "image/avif", "renditions/output.avif"); err != nil {
		t.Fatal(err)
	}
	if err := validatePinnedOutput(pinned, "active", "image/gif", "image/avif", "renditions/output.avif"); err == nil {
		t.Fatal("animated GIF standard output unexpectedly accepted as AVIF")
	}
}
