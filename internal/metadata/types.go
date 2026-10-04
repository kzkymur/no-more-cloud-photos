// Package metadata detects and extracts bounded metadata from complete media files.
package metadata

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/kzkymur/no-more-cloud-photos/internal/mediaformat"
)

var (
	ErrUnsupportedMediaType = errors.New("unsupported media type")
	ErrInvalidMedia         = errors.New("invalid media")
	ErrPolicyViolation      = errors.New("metadata policy violation")
	ErrProbeTimeout         = errors.New("metadata probe timeout")
	ErrProbeOutputLimit     = errors.New("metadata probe output limit")
	ErrProbeFailed          = errors.New("metadata probe failed")
	ErrMaxDimension         = errors.New("maximum dimension exceeded")
	ErrMaxPixels            = errors.New("maximum pixel count exceeded")
)

const (
	TakenAtUnknown         = "unknown"
	TakenAtEmbeddedOffset  = "embedded_offset"
	TakenAtDefaultTimezone = "default_timezone"
)

type Policy struct {
	Timeout           time.Duration
	AddressSpaceBytes uint64
	OutputBytes       int64
	MaxDimension      int
	MaxPixels         uint64
}

func DefaultPolicy() Policy {
	return Policy{
		Timeout:           60 * time.Second,
		AddressSpaceBytes: 1 << 30,
		OutputBytes:       1 << 20,
		MaxDimension:      100_000,
		MaxPixels:         1_000_000_000,
	}
}

func (p Policy) Validate() error {
	if p.Timeout < time.Second || p.Timeout > 10*time.Minute {
		return fmt.Errorf("metadata timeout must be between 1s and 10m")
	}
	if p.AddressSpaceBytes < 64<<20 || p.AddressSpaceBytes > 16<<30 {
		return fmt.Errorf("metadata address-space limit must be between 64 MiB and 16 GiB")
	}
	if p.OutputBytes < 1024 || p.OutputBytes > 16<<20 {
		return fmt.Errorf("metadata output limit must be between 1 KiB and 16 MiB")
	}
	if p.MaxDimension < 1 || p.MaxDimension > 1_000_000 {
		return fmt.Errorf("metadata maximum dimension must be between 1 and 1000000")
	}
	if p.MaxPixels < 1 || p.MaxPixels > 1_000_000_000_000 {
		return fmt.Errorf("metadata maximum pixels must be between 1 and 1000000000000")
	}
	return nil
}

type Detection struct {
	Format   mediaformat.Format
	Evidence string
}

type CaptureCandidate struct {
	Name          string `json:"name"`
	Kind          string `json:"kind"`
	DateTime      string `json:"date_time"`
	Offset        string `json:"offset,omitempty"`
	OffsetPresent bool   `json:"offset_present,omitempty"`
	OffsetOK      bool   `json:"offset_valid,omitempty"`
	Discarded     string `json:"discarded_reason,omitempty"`
}

type Capture struct {
	TakenAt       *time.Time         `json:"-"`
	Source        string             `json:"source"`
	Timezone      *string            `json:"timezone,omitempty"`
	Selected      *CaptureCandidate  `json:"selected,omitempty"`
	SelectedIndex *int               `json:"selected_index,omitempty"`
	Candidates    []CaptureCandidate `json:"candidates,omitempty"`
	FoldDecision  string             `json:"fold_decision,omitempty"`
}

// SourceEXIF is an allowlist, not a complete copy of the source EXIF block.
// Original bytes are never modified by this package.
type SourceEXIF struct {
	Make           *string  `json:"make,omitempty"`
	Model          *string  `json:"model,omitempty"`
	LensMake       *string  `json:"lens_make,omitempty"`
	LensModel      *string  `json:"lens_model,omitempty"`
	Orientation    *int     `json:"orientation,omitempty"`
	ExposureTime   *string  `json:"exposure_time,omitempty"`
	Aperture       *float64 `json:"aperture,omitempty"`
	ISO            *int     `json:"iso,omitempty"`
	FocalLengthMM  *float64 `json:"focal_length_mm,omitempty"`
	GPSLatitude    *float64 `json:"gps_latitude,omitempty"`
	GPSLongitude   *float64 `json:"gps_longitude,omitempty"`
	GPSAltitudeM   *float64 `json:"gps_altitude_m,omitempty"`
	DateTimeRaw    *string  `json:"date_time_raw,omitempty"`
	DateTimeOffset *string  `json:"date_time_offset_raw,omitempty"`
}

type DerivedMetadata struct {
	DetectorEvidence string  `json:"detector_evidence"`
	ProbeTool        string  `json:"probe_tool"`
	ProbeVersion     string  `json:"probe_version"`
	PrimaryStream    *int    `json:"primary_stream,omitempty"`
	VideoCodec       string  `json:"video_codec,omitempty"`
	Capture          Capture `json:"capture"`
}

type Result struct {
	MIMEType   string
	Extension  string
	Width      int
	Height     int
	DurationMS *int64
	EXIF       SourceEXIF
	Derived    DerivedMetadata
}

func (r Result) EXIFJSON() ([]byte, error) {
	return json.Marshal(r.EXIF)
}

func (r Result) SourceMetadataJSON() ([]byte, error) {
	return json.Marshal(r.Derived)
}
