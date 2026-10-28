package modes

import "github.com/patriceckhart/zot/packages/tui"

// Ignore stale mouse reports without turning them into editor or history input.
// Mouse reporting stays disabled so the terminal owns scrolling and selection.
func (i *Interactive) handleMouseKey(k tui.Key) bool {
	switch k.Kind {
	case tui.KeyMouseLeft, tui.KeyMouseDrag, tui.KeyMouseRelease,
		tui.KeyMouseWheelUp, tui.KeyMouseWheelDown:
		return true
	default:
		return false
	}
}
