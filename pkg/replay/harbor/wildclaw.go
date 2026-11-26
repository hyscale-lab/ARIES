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

// Only these digest-reviewed local file operations may use the generic document hooks.
func localFileSource(sha string) bool {
	switch sha {
	case "56ef19ba1c9b3096dab3522e9d45080fc5457dabef0082fbaeaae0d1e6fddc68",
		"6811509f8ac7460ddcd9be97b3a68166384941e8795837a0b3365560a24cfb61",
		"6fdb46f630e5f42e9b7dd8225f8c59785e6bc9e8d8881b3b3744a57d8cfbaada",
		"f9c8fb5e9bb4b9dc90005a65e86952af6a7adbc78a616c81f1c1cfaead2a47db",
		"299e58cc16a532659b379102be2543c43f6dd24f397b94f460b04dc4d42dc25c",
		"f4d335cb20dbe8614dc7cc268634128d26eba95bb4719dbf32b229d5e12d7ffd",
		"687aa2a8b04cdb4937c23afca77428cd6a194bb141af772453d988a06794e136",
		"aa69da99ce7d25f0e9e591183a395ad1538c121e5a7d23cf3b8d1bc781c13502",
		"0f72cc52fae9f4eeb833fcad744dc7af0e8f9f8717038cf54dc59849732aed15",
		"4d94b36f689fd172277244f8542f630bb91e33a6235c95f1a337c9a6f5359ff0",
		"167633cdb65195ec4678a81f5ab3820a7cc14cffd2ca8cb64944cbda62148556",
		"1b2d1be3129789ab9a5f81dcd4f2949a7d2391fda68bbbe735d1941af3fbb8ee",
		"66f0fee0f3847dccfdecf1687ffdc3104b9d9a5d323dc71bfb0116e69871064d",
		"b81663dd33480607ca01403d075a5a2cfa10a1fce0945df9d0c17cb9ac597f25",
		"653b6bbe76655ec283f849e2d4f731f78e5b7adb78ea11ff4cbc833d297db0fe",
		"dce6d103d9d7fb344b44a6e4b37bab46461e92f89e8960ca3af5907931f1c3bd":
		return true
	}
	return false
}

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
