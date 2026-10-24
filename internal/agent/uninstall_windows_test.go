package agent

import (
	"os"
	"testing"
)

// On a machine with the agent installed from its MSI (lab check:
// BACKUPZIT_TEST_PRODUCTCODE=1), the uninstaller finds the product code.
func TestAgentProductCodeLive(t *testing.T) {
	if os.Getenv("BACKUPZIT_TEST_PRODUCTCODE") == "" {
		t.Skip("set BACKUPZIT_TEST_PRODUCTCODE=1 on a machine with the agent installed")
	}
	code, err := agentProductCode()
	if err != nil || !msiProductCode.MatchString(code) {
		t.Fatalf("product code %q: %v", code, err)
	}
	t.Logf("product code %s", code)
}
