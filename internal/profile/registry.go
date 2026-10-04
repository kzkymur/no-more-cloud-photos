package profile

import (
	"fmt"
	"math"
	"strings"

	"github.com/kzkymur/no-more-cloud-photos/internal/mediaformat"
)

type Capability struct {
	MIMEType   string
	SourceMode SourceMode
}

type OutputKind string

const (
	OutputStillAVIF     OutputKind = "still-avif"
	OutputAnimationWebP OutputKind = "animation-webp"
	OutputVideoAV1      OutputKind = "video-av1"
)

// Certification records a proven recipe option envelope for one candidate input.
type Certification struct {
	MIMEType           string
	SourceMode         SourceMode
	OutputKind         OutputKind
	MaxLongEdgeCeiling int
	SettingMin         int
	SettingMax         int
	Evidence           string
}

// Registry is an immutable candidate membership and certification registry.
type Registry struct {
	capabilities   map[string]Capability
	certifications []Certification
}

var candidateCapabilities = func() []Capability {
	formats := mediaformat.Formats()
	result := make([]Capability, 0, len(formats))
	for _, format := range formats {
		var sourceMode SourceMode
		switch format.Family {
		case mediaformat.FamilyStill:
			sourceMode = SourceStill
		case mediaformat.FamilyProbeAnimation:
			sourceMode = SourceProbeAnimation
		case mediaformat.FamilyVideo:
			sourceMode = SourceVideo
		default:
			panic("unknown media format family")
		}
		result = append(result, Capability{MIMEType: format.MIMEType, SourceMode: sourceMode})
	}
	return result
}()

// CandidateRegistry returns the fixed provisional MIME registry without certifications.
func CandidateRegistry() Registry {
	capabilities := make(map[string]Capability, len(candidateCapabilities))
	for _, capability := range candidateCapabilities {
		capabilities[capability.MIMEType] = capability
	}
	return Registry{capabilities: capabilities}
}

// Capabilities returns candidate memberships in stable MIME-sorted order.
func (r Registry) Capabilities() []Capability {
	result := make([]Capability, 0, len(candidateCapabilities))
	for _, candidate := range candidateCapabilities {
		if capability, ok := r.capabilities[candidate.MIMEType]; ok {
			result = append(result, capability)
		}
	}
	return result
}

// Certifications returns an independent copy in insertion order.
func (r Registry) Certifications() []Certification {
	return append([]Certification(nil), r.certifications...)
}

// WithCertification returns a new registry containing one proven option envelope.
func (r Registry) WithCertification(certification Certification) (Registry, error) {
	capability, ok := r.capabilities[certification.MIMEType]
	if !ok {
		return Registry{}, fmt.Errorf("unknown candidate MIME type %q", certification.MIMEType)
	}
	if capability.SourceMode != certification.SourceMode {
		return Registry{}, fmt.Errorf("source mode %q does not match candidate mode %q", certification.SourceMode, capability.SourceMode)
	}
	if !allowedOutputKind(certification.SourceMode, certification.OutputKind) {
		return Registry{}, fmt.Errorf("output kind %q is not allowed for source mode %q", certification.OutputKind, certification.SourceMode)
	}
	if certification.MaxLongEdgeCeiling < 1 || certification.MaxLongEdgeCeiling > math.MaxInt32 {
		return Registry{}, fmt.Errorf("max long edge ceiling must be between 1 and %d", math.MaxInt32)
	}
	minimum, maximum := settingBounds(certification.OutputKind)
	if certification.SettingMin < minimum || certification.SettingMax > maximum || certification.SettingMin > certification.SettingMax {
		return Registry{}, fmt.Errorf("setting range must be within %d..%d", minimum, maximum)
	}
	if evidence := strings.Trim(certification.Evidence, " \t\n\r\f\v"); evidence == "" || evidence == EvidenceProvisional {
		return Registry{}, fmt.Errorf("certification evidence must not be empty")
	}

	capabilities := make(map[string]Capability, len(r.capabilities))
	for mimeType, current := range r.capabilities {
		capabilities[mimeType] = current
	}
	certifications := append([]Certification(nil), r.certifications...)
	certifications = append(certifications, certification)
	return Registry{capabilities: capabilities, certifications: certifications}, nil
}

func (r Registry) capability(mimeType string) (Capability, bool) {
	capability, ok := r.capabilities[mimeType]
	return capability, ok
}

func (r Registry) certifies(mimeType string, sourceMode SourceMode, outputKind OutputKind, maxLongEdge, setting int) bool {
	for _, certification := range r.certifications {
		if certification.MIMEType == mimeType && certification.SourceMode == sourceMode && certification.OutputKind == outputKind &&
			maxLongEdge <= certification.MaxLongEdgeCeiling && setting >= certification.SettingMin && setting <= certification.SettingMax {
			return true
		}
	}
	return false
}

func allowedOutputKind(sourceMode SourceMode, outputKind OutputKind) bool {
	switch sourceMode {
	case SourceStill:
		return outputKind == OutputStillAVIF
	case SourceProbeAnimation:
		return outputKind == OutputStillAVIF || outputKind == OutputAnimationWebP
	case SourceVideo:
		return outputKind == OutputStillAVIF || outputKind == OutputVideoAV1
	default:
		return false
	}
}

func settingBounds(outputKind OutputKind) (int, int) {
	if outputKind == OutputVideoAV1 {
		return 0, 63
	}
	return 1, 100
}
