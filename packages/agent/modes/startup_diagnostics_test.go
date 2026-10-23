package modes

import (
	"fmt"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/core"
	"github.com/patriceckhart/zot/packages/provider"
)

func TestStartupDiagnosticsDoNotHideLatestReply(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("streaming=%v", streaming), func(t *testing.T) {
			var diagnostics []string
			for n := 0; n < 100; n++ {
				diagnostics = append(diagnostics, fmt.Sprintf("skill discovery: skill %d shadowed", n))
			}
			agent := core.NewAgent(nil, "", "", nil)
			agent.SetMessages([]provider.Message{{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "hello"}}}})
			i := NewInteractive(InteractiveConfig{Agent: agent, StartupExtensionErrors: diagnostics})
			const reply = "visible model reply"
			if streaming {
				i.streamOn = true
				i.streaming.WriteString(reply)
			} else {
				agent.SetMessages(append(agent.Messages(), provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: reply}}}))
			}
			chat := i.buildChatLocked(60)
			// The viewport follows the final rows of the chat buffer.
			tail := strings.Join(chat[max(0, len(chat)-10):], "\n")
			if !strings.Contains(tail, reply) {
				t.Fatalf("latest reply hidden by diagnostics: %q", tail)
			}
			if !strings.Contains(strings.Join(chat, "\n"), diagnostics[0]) {
				t.Fatal("startup diagnostics were discarded")
			}
			wantMessages := 2
			if streaming {
				wantMessages = 1
			}
			if len(agent.Messages()) != wantMessages {
				t.Fatal("diagnostics entered model context")
			}
		})
	}
}
