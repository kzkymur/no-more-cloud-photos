// Package mediaformat owns the closed, content-detected input media registry.
package mediaformat

import "sort"

type Family string

const (
	FamilyStill          Family = "still"
	FamilyProbeAnimation Family = "probe-animation"
	FamilyVideo          Family = "video"
)

type Format struct {
	MIMEType  string
	Extension string
	Family    Family
}

var formats = []Format{
	{MIMEType: "image/bmp", Extension: "bmp", Family: FamilyStill},
	{MIMEType: "image/dng", Extension: "dng", Family: FamilyStill},
	{MIMEType: "image/gif", Extension: "gif", Family: FamilyProbeAnimation},
	{MIMEType: "image/heic", Extension: "heic", Family: FamilyStill},
	{MIMEType: "image/heif", Extension: "heif", Family: FamilyStill},
	{MIMEType: "image/jpeg", Extension: "jpg", Family: FamilyStill},
	{MIMEType: "image/png", Extension: "png", Family: FamilyStill},
	{MIMEType: "image/webp", Extension: "webp", Family: FamilyProbeAnimation},
	{MIMEType: "image/x-canon-cr2", Extension: "cr2", Family: FamilyStill},
	{MIMEType: "image/x-canon-cr3", Extension: "cr3", Family: FamilyStill},
	{MIMEType: "image/x-fuji-raf", Extension: "raf", Family: FamilyStill},
	{MIMEType: "image/x-nikon-nef", Extension: "nef", Family: FamilyStill},
	{MIMEType: "image/x-olympus-orf", Extension: "orf", Family: FamilyStill},
	{MIMEType: "image/x-panasonic-rw2", Extension: "rw2", Family: FamilyStill},
	{MIMEType: "image/x-sony-arw", Extension: "arw", Family: FamilyStill},
	{MIMEType: "video/mp4", Extension: "mp4", Family: FamilyVideo},
	{MIMEType: "video/quicktime", Extension: "mov", Family: FamilyVideo},
}

var byMIME = func() map[string]Format {
	result := make(map[string]Format, len(formats))
	for _, format := range formats {
		result[format.MIMEType] = format
	}
	return result
}()

// Formats returns an independent MIME-sorted registry snapshot.
func Formats() []Format {
	result := append([]Format(nil), formats...)
	sort.Slice(result, func(i, j int) bool { return result[i].MIMEType < result[j].MIMEType })
	return result
}

func Lookup(mimeType string) (Format, bool) {
	format, ok := byMIME[mimeType]
	return format, ok
}
