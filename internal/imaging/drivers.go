package imaging

import "strings"

// LoadDriversResult says which INF files drvload loaded.
type LoadDriversResult struct {
	Loaded []string `json:"loaded"`
	Failed []string `json:"failed,omitempty"`
}

// ProblemDevice is a device Windows has no working driver for.
type ProblemDevice struct {
	InstanceID  string `json:"instance_id"`
	Description string `json:"description,omitempty"`
	Class       string `json:"class,omitempty"`
	Problem     string `json:"problem,omitempty"`
}

// parsePnputilDevices reads the output of "pnputil /enum-devices": blocks
// of "Key: Value" lines separated by blank lines. The keys are localized,
// so the fields are taken by position: instance ID first, then
// description, class name, ... and a problem line if there is one.
func parsePnputilDevices(out string) []ProblemDevice {
	var devs []ProblemDevice
	var cur []string
	flush := func() {
		if len(cur) == 0 {
			return
		}
		d := ProblemDevice{InstanceID: cur[0]}
		if len(cur) > 1 {
			d.Description = cur[1]
		}
		if len(cur) > 2 {
			d.Class = cur[2]
		}
		// A block may be shorter than three fields (found by fuzzing).
		for _, v := range cur[min(3, len(cur)):] {
			if strings.Contains(v, "CM_PROB_") || strings.HasPrefix(v, "0x") || strings.Contains(strings.ToLower(v), "problem") {
				d.Problem = v
			}
		}
		devs = append(devs, d)
		cur = nil
	}
	for _, line := range strings.Split(strings.ReplaceAll(out, "\r\n", "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		// The header ("Microsoft PnP Utility") has no value.
		if v = strings.TrimSpace(v); v == "" && len(cur) == 0 {
			continue
		}
		if len(cur) == 0 && !strings.Contains(v, `\`) {
			// The first field of a block is the instance ID (bus\device\instance).
			continue
		}
		_ = k
		cur = append(cur, v)
	}
	flush()
	return devs
}
