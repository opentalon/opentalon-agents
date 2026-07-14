package agent

import (
	"fmt"

	"github.com/robfig/cron/v3"
)

// ParseCron parses a standard 5-field cron expression (minute hour dom mon dow).
func ParseCron(spec string) (cron.Schedule, error) {
	s, err := cron.ParseStandard(spec)
	if err != nil {
		return nil, fmt.Errorf("invalid cron %q: %w", spec, err)
	}
	return s, nil
}

// ScheduleTrigger returns the agent's first schedule trigger's cron, if any.
func (a Agent) ScheduleTrigger() (string, bool) {
	for _, t := range a.Triggers {
		if t.Type == TriggerSchedule && t.Cron != "" {
			return t.Cron, true
		}
	}
	return "", false
}

// ValidateTriggers checks each trigger's config so authoring-time mistakes
// (bad cron, bad interval, missing mapping) are rejected up front rather
// than failing silently at tick time.
func ValidateTriggers(triggers []Trigger) error {
	for _, t := range triggers {
		switch t.Type {
		case TriggerSchedule:
			if t.Cron == "" {
				return fmt.Errorf("schedule trigger requires a cron expression")
			}
			if _, err := ParseCron(t.Cron); err != nil {
				return err
			}
		case TriggerPoll:
			pc, err := t.Poll()
			if err != nil {
				return err
			}
			if pc.Server == "" || pc.Tool == "" {
				return fmt.Errorf("poll trigger requires server and tool")
			}
			if pc.ValuePath == "" || pc.Attribute == "" {
				return fmt.Errorf("poll trigger requires value_path and attribute")
			}
			if _, err := pc.IntervalDuration(); err != nil {
				return fmt.Errorf("poll interval: %w", err)
			}
		case TriggerWebhook:
			wc, err := t.Webhook()
			if err != nil {
				return err
			}
			if wc.ValuePath == "" || wc.Attribute == "" {
				return fmt.Errorf("webhook trigger requires value_path and attribute")
			}
		case TriggerManual, "":
			// nothing to validate
		default:
			return fmt.Errorf("unknown trigger type %q", t.Type)
		}
	}
	return nil
}
