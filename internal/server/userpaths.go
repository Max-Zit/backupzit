package server

import (
	"fmt"

	"github.com/max-zit/backupzit/internal/fsutil"
)

// minPatternVersion is the first agent version that expands {users} and
// wildcards in the folders of a files job.
const minPatternVersion = "0.45.0"

// checkSourcePatterns refuses {users} and wildcard sources where they
// cannot work: in jobs other than files jobs, and on older agents (they
// would look for a folder literally named "{users}").
func checkSourcePatterns(agent Agent, j *Job) error {
	if j.Kind != JobFiles && j.Kind != JobNAS {
		return nil
	}
	for _, p := range j.Paths {
		if !fsutil.IsPattern(p) {
			continue
		}
		if j.Kind == JobNAS {
			return fmt.Errorf("%s: {users} and wildcards work in files jobs, not for NAS shares", p)
		}
		if agentOlder(agent, minPatternVersion) {
			return fmt.Errorf("the agent on %s (%s) cannot expand %s yet; update it to %s or newer", agent.Hostname, agent.Version, p, minPatternVersion)
		}
	}
	return nil
}
