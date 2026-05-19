// Package context provides context lifecycle monitoring types, including
// per-topic override support (M9).
package context

import (
	"encoding/json"

	"github.com/jaekwon-park/tgcc/internal/config"
)

// ContextOverrides represents topic-level context threshold overrides.
// Fields are pointers so we can distinguish "unset" (nil) from "set to zero".
type ContextOverrides struct {
	SoftWarnBytes    *int64 `json:"soft_warn_bytes,omitempty"`
	HardCompactBytes *int64 `json:"hard_compact_bytes,omitempty"`
}

// ParseOverrides unmarshals a JSON string into ContextOverrides.
// Returns nil, nil if jsonStr is empty.
func ParseOverrides(jsonStr string) (*ContextOverrides, error) {
	if jsonStr == "" {
		return nil, nil
	}
	var o ContextOverrides
	if err := json.Unmarshal([]byte(jsonStr), &o); err != nil {
		return nil, err
	}
	return &o, nil
}

// ToJSON marshals ContextOverrides to a JSON string.
// Returns empty string if o is nil.
func (o *ContextOverrides) ToJSON() (string, error) {
	if o == nil {
		return "", nil
	}
	b, err := json.Marshal(o)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// MergeWithGlobal applies topic-level overrides on top of the global config.
// Only non-nil, positive override values replace the global defaults.
func MergeWithGlobal(overrides *ContextOverrides, global config.ContextConfig) config.ContextConfig {
	merged := global
	if overrides == nil {
		return merged
	}
	if overrides.SoftWarnBytes != nil && *overrides.SoftWarnBytes > 0 {
		merged.SoftWarnBytes = *overrides.SoftWarnBytes
	}
	if overrides.HardCompactBytes != nil && *overrides.HardCompactBytes > 0 {
		merged.HardCompactBytes = *overrides.HardCompactBytes
	}
	return merged
}
