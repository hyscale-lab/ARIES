package harbor

import (
	"errors"
	"time"
)

const (
	wildClawCalendarDigest      = "105845c532ae7051e61f44a6c46367e63081d2973cdb5d8d057c3abc85c44180"
	wildClawImageCategoryDigest = "2543f544fef10d6ee0da62095eb37bdfc9ba9f4d5998db798e7a54e85c0918ab"
	wildClawPuzzleDigest        = "0f37ca65135cf0513ada9eef9fe1cc62dc8751cdc0f7fa872ad06d6e68e27709"
	wildClawDatasetRevision     = "f2cafb4e53d0f5ae8edd2097c3b60276ecf2d04f"
)

// OpenClawTrace identifies the native session and the observed portion of its timeline.
type OpenClawTrace struct {
	Version         int       `json:"version"`
	Revision        string    `json:"dataset_revision,omitempty"`
	SHA256          string    `json:"sha256"`
	RunID           string    `json:"run_id"`
	CWD             string    `json:"session_cwd"`
	Start           time.Time `json:"user_prompt_time"`
	End             time.Time `json:"final_response_time,omitzero"`
	ObservedThrough time.Time `json:"last_record_time"`
	Status          string    `json:"trace_status"`
	StatusReason    string    `json:"trace_status_reason,omitempty"`
	UnansweredCalls []string  `json:"unanswered_call_ids,omitempty"`
}

// LoadWildClawCalendar preserves the original calendar-only entry point.
func LoadWildClawCalendar(trace []byte) (*Recording, error) {
	if digest(trace) != wildClawCalendarDigest {
		return nil, errors.New("source bytes do not match the reviewed WildClawBench Sol calendar trace")
	}
	return LoadWildClawLive(trace)
}
