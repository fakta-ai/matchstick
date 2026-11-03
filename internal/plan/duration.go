package plan

import (
	"encoding/json"
	"fmt"
	"time"
)

// Duration is a time.Duration written in JSON as Go's duration text, such as "10m" or "3h".
type Duration time.Duration

// MarshalJSON writes the duration as text.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON reads the duration from text.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("a duration is text such as \"10m\": %w", err)
	}
	parsed, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(parsed)
	return nil
}
