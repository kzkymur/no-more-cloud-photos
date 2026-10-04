// Package readapi defines the typed contracts shared by the read service and
// its eventual HTTP adapter.
package readapi

import (
	"encoding/json"
	"fmt"
	"regexp"
	"time"
)

const (
	DefaultLimit      = 50
	MaxLimit          = 200
	DefaultProfileKey = "standard"
)

type DeletedFilter string

const (
	DeletedExclude DeletedFilter = "exclude"
	DeletedOnly    DeletedFilter = "only"
	DeletedInclude DeletedFilter = "include"
)

func (f DeletedFilter) Valid() bool {
	switch f {
	case DeletedExclude, DeletedOnly, DeletedInclude:
		return true
	default:
		return false
	}
}

func ParseDeletedFilter(value string) (DeletedFilter, error) {
	filter := DeletedFilter(value)
	if !filter.Valid() {
		return "", fmt.Errorf("invalid deleted filter %q", value)
	}
	return filter, nil
}

type JobStatus string

const (
	JobQueued    JobStatus = "queued"
	JobRunning   JobStatus = "running"
	JobSucceeded JobStatus = "succeeded"
	JobFailed    JobStatus = "failed"
	JobCancelled JobStatus = "cancelled"
)

func (s JobStatus) Valid() bool {
	switch s {
	case JobQueued, JobRunning, JobSucceeded, JobFailed, JobCancelled:
		return true
	default:
		return false
	}
}

func ParseJobStatus(value string) (JobStatus, error) {
	status := JobStatus(value)
	if !status.Valid() {
		return "", fmt.Errorf("invalid job status %q", value)
	}
	return status, nil
}

type ProfileStatus string

const (
	ProfileDraft   ProfileStatus = "draft"
	ProfileActive  ProfileStatus = "active"
	ProfileRetired ProfileStatus = "retired"
)

func (s ProfileStatus) Valid() bool {
	switch s {
	case ProfileDraft, ProfileActive, ProfileRetired:
		return true
	default:
		return false
	}
}

func ParseProfileStatus(value string) (ProfileStatus, error) {
	status := ProfileStatus(value)
	if !status.Valid() {
		return "", fmt.Errorf("invalid profile status %q", value)
	}
	return status, nil
}

type JobType string

const (
	JobTransform JobType = "transform"
	JobPurge     JobType = "purge"
)

type JobTargetStatus string

const (
	TargetPending   JobTargetStatus = "pending"
	TargetSucceeded JobTargetStatus = "succeeded"
	TargetFailed    JobTargetStatus = "failed"
)

type TakenAtSource string

const (
	TakenAtEmbeddedOffset  TakenAtSource = "embedded_offset"
	TakenAtDefaultTimezone TakenAtSource = "default_timezone"
	TakenAtUnknown         TakenAtSource = "unknown"
)

type ProfileRef struct {
	ID      string `json:"id"`
	Key     string `json:"key"`
	Version int    `json:"version"`
}

type Rendition struct {
	ID          string     `json:"id"`
	MediaID     string     `json:"media_id"`
	JobTargetID string     `json:"job_target_id"`
	Profile     ProfileRef `json:"profile"`
	MIMEType    string     `json:"mime_type"`
	SizeBytes   int64      `json:"size_bytes"`
	Width       *int       `json:"width"`
	Height      *int       `json:"height"`
	DurationMS  *int64     `json:"duration_ms"`
	SHA256      string     `json:"sha256"`
	FileURL     string     `json:"file_url"`
	CreatedAt   time.Time  `json:"created_at"`
}

type MediaSummary struct {
	ID               string        `json:"id"`
	MIMEType         string        `json:"mime_type"`
	OriginalFilename *string       `json:"original_filename"`
	SizeBytes        int64         `json:"size_bytes"`
	Width            *int          `json:"width"`
	Height           *int          `json:"height"`
	DurationMS       *int64        `json:"duration_ms"`
	TakenAt          *time.Time    `json:"taken_at"`
	TakenAtSource    TakenAtSource `json:"taken_at_source"`
	TakenAtTimezone  *string       `json:"taken_at_timezone"`
	CreatedAt        time.Time     `json:"created_at"`
	DeletedAt        *time.Time    `json:"deleted_at"`
	PurgeAfter       *time.Time    `json:"purge_after"`
	Rendition        *Rendition    `json:"rendition"`
}

type MediaDetail struct {
	ID                string        `json:"id"`
	MIMEType          string        `json:"mime_type"`
	OriginalFilename  *string       `json:"original_filename"`
	SizeBytes         int64         `json:"size_bytes"`
	Width             *int          `json:"width"`
	Height            *int          `json:"height"`
	DurationMS        *int64        `json:"duration_ms"`
	TakenAt           *time.Time    `json:"taken_at"`
	TakenAtSource     TakenAtSource `json:"taken_at_source"`
	TakenAtTimezone   *string       `json:"taken_at_timezone"`
	CreatedAt         time.Time     `json:"created_at"`
	DeletedAt         *time.Time    `json:"deleted_at"`
	PurgeAfter        *time.Time    `json:"purge_after"`
	CurrentRenditions []Rendition   `json:"current_renditions"`
	Jobs              []Job         `json:"jobs"`
}

func NewMediaDetail() MediaDetail {
	return MediaDetail{CurrentRenditions: []Rendition{}, Jobs: []Job{}}
}

func (m *MediaDetail) InitializeArrays() {
	if m.CurrentRenditions == nil {
		m.CurrentRenditions = []Rendition{}
	}
	if m.Jobs == nil {
		m.Jobs = []Job{}
	}
	for i := range m.Jobs {
		m.Jobs[i].InitializeArrays()
	}
}

type Original struct {
	ID               string    `json:"id"`
	MediaID          string    `json:"media_id"`
	MIMEType         string    `json:"mime_type"`
	SizeBytes        int64     `json:"size_bytes"`
	SHA256           string    `json:"sha256"`
	OriginalFilename *string   `json:"original_filename"`
	FileURL          string    `json:"file_url"`
	CreatedAt        time.Time `json:"created_at"`
}

type ResourceError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type Job struct {
	ID           string         `json:"id"`
	Type         JobType        `json:"type"`
	Status       JobStatus      `json:"status"`
	MediaID      string         `json:"media_id"`
	OriginalID   *string        `json:"original_id"`
	Attempts     int            `json:"attempts"`
	MaxAttempts  int            `json:"max_attempts"`
	AvailableAt  time.Time      `json:"available_at"`
	StartedAt    *time.Time     `json:"started_at"`
	FinishedAt   *time.Time     `json:"finished_at"`
	Error        *ResourceError `json:"error"`
	CancelledAt  *time.Time     `json:"cancelled_at"`
	CancelReason *string        `json:"cancel_reason"`
	CreatedAt    time.Time      `json:"created_at"`
	UpdatedAt    time.Time      `json:"updated_at"`
	Targets      []JobTarget    `json:"targets"`
}

func NewJob() Job {
	return Job{Targets: []JobTarget{}}
}

func (j *Job) InitializeArrays() {
	if j.Targets == nil {
		j.Targets = []JobTarget{}
	}
}

type JobTarget struct {
	ID          string          `json:"id"`
	Profile     ProfileRef      `json:"profile"`
	Status      JobTargetStatus `json:"status"`
	Attempts    int             `json:"attempts"`
	Error       *ResourceError  `json:"error"`
	RenditionID *string         `json:"rendition_id"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

type Profile struct {
	ID                      string          `json:"id"`
	Key                     string          `json:"key"`
	Version                 int             `json:"version"`
	Status                  ProfileStatus   `json:"status"`
	InputMIMETypes          []string        `json:"input_mime_types"`
	ParametersSchemaVersion int             `json:"parameters_schema_version"`
	Processor               string          `json:"processor"`
	Parameters              json.RawMessage `json:"parameters"`
	CreatedAt               time.Time       `json:"created_at"`
	ActivatedAt             *time.Time      `json:"activated_at"`
	RetiredAt               *time.Time      `json:"retired_at"`
}

func NewProfile() Profile {
	return Profile{InputMIMETypes: []string{}}
}

func (p *Profile) InitializeArrays() {
	if p.InputMIMETypes == nil {
		p.InputMIMETypes = []string{}
	}
}

type MediaPage struct {
	Items      []MediaSummary `json:"items"`
	NextCursor *string        `json:"next_cursor"`
}

func NewMediaPage(items []MediaSummary, nextCursor *string) MediaPage {
	if items == nil {
		items = []MediaSummary{}
	}
	return MediaPage{Items: items, NextCursor: nextCursor}
}

type JobPage struct {
	Items      []Job   `json:"items"`
	NextCursor *string `json:"next_cursor"`
}

func NewJobPage(items []Job, nextCursor *string) JobPage {
	if items == nil {
		items = []Job{}
	}
	for i := range items {
		items[i].InitializeArrays()
	}
	return JobPage{Items: items, NextCursor: nextCursor}
}

type ProfilePage struct {
	Items []Profile `json:"items"`
}

func NewProfilePage(items []Profile) ProfilePage {
	if items == nil {
		items = []Profile{}
	}
	for i := range items {
		items[i].InitializeArrays()
	}
	return ProfilePage{Items: items}
}

type MediaListRequest struct {
	Profile string
	Deleted DeletedFilter
	Cursor  string
	Limit   int
}

func NewMediaListRequest() MediaListRequest {
	return MediaListRequest{Profile: DefaultProfileKey, Deleted: DeletedExclude, Limit: DefaultLimit}
}

func (r MediaListRequest) Validate() error {
	fields := make(map[string]string)
	if !validProfileKey(r.Profile) {
		fields["profile"] = "invalid"
	}
	if !r.Deleted.Valid() {
		fields["deleted"] = "invalid"
	}
	validateLimit(r.Limit, fields)
	return validationError(fields)
}

type JobListRequest struct {
	Status  JobStatus
	MediaID string
	Cursor  string
	Limit   int
}

func NewJobListRequest() JobListRequest {
	return JobListRequest{Limit: DefaultLimit}
}

func (r JobListRequest) Validate() error {
	fields := make(map[string]string)
	if r.Status != "" && !r.Status.Valid() {
		fields["status"] = "invalid"
	}
	if r.MediaID != "" && !IsUUIDv4(r.MediaID) {
		fields["media_id"] = "invalid"
	}
	validateLimit(r.Limit, fields)
	return validationError(fields)
}

type ProfileListRequest struct {
	Status ProfileStatus
}

func (r ProfileListRequest) Validate() error {
	if r.Status == "" || r.Status.Valid() {
		return nil
	}
	return NewInvalidRequest(map[string]string{"status": "invalid"})
}

var profileKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

func validProfileKey(value string) bool {
	return profileKeyPattern.MatchString(value)
}

func validateLimit(limit int, fields map[string]string) {
	if limit < 1 {
		fields["limit"] = "invalid"
	} else if limit > MaxLimit {
		fields["limit"] = "out_of_range"
	}
}

func validationError(fields map[string]string) error {
	if len(fields) == 0 {
		return nil
	}
	return NewInvalidRequest(fields)
}
