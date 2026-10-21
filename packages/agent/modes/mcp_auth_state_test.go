package modes

import (
	"testing"

	"github.com/patriceckhart/zot/packages/mcp"
	"github.com/patriceckhart/zot/packages/tui"
)

func TestMCPManagerAllowsSignInWithRejectedStoredToken(t *testing.T) {
	d := managerFixture(mcp.ServerInfo{
		Name: "secure", State: mcp.StateSignIn,
		OAuth: true, SignedIn: true,
	})
	action := d.HandleKey(tui.Key{Kind: tui.KeyRune, Rune: 'l'})
	if action.Kind != mcpActionSignIn || action.Name != "secure" {
		t.Fatalf("stored token hid the required sign-in action: %+v", action)
	}
}
