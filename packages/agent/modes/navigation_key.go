package modes

import "github.com/patriceckhart/zot/packages/tui"

// navigationContextActive excludes text-only subviews from navigation actions.
// Filterable lists still own navigation while their filter is being edited.
func (i *Interactive) navigationContextActive() bool {
	if i.confirmDialog.Focused() {
		return true
	}
	if i.dialog.Active() {
		return i.dialog.step == loginStepMethod || i.dialog.step == loginStepProvider || i.dialog.step == loginStepOAuthMethod
	}
	if i.sessionDialog.Active() {
		return !i.sessionDialog.renaming && !i.sessionDialog.deleting
	}
	if i.swarmDialog.Active() {
		d := i.swarmDialog
		if d.pickingModel {
			return true
		}
		if d.spawning {
			return fileNavigationActive(d.newTaskEd, d.fileSuggest)
		}
		if d.prompting || d.viewing {
			return fileNavigationActive(d.promptEd, d.promptSuggest)
		}
		return true
	}
	if i.btwDialog.Active() {
		d := i.btwDialog
		d.mu.Lock()
		defer d.mu.Unlock()
		return fileNavigationActive(d.editor, d.fileSuggest)
	}
	return i.dialogOwnsInput() && !i.extPanel.Active()
}

func fileNavigationActive(ed *tui.Editor, fs *fileSuggester) bool {
	return ed != nil && fs != nil && fs.Active(ed.Value())
}

// navigationKey converts configured navigation actions to the existing arrow
// behavior. Commands and unbound keys retain their original representation.
func navigationKey(bindings []keymapBinding, k tui.Key) tui.Key {
	if keymapReservedKey(k) {
		return k
	}
	binding, _ := lookupKeyBinding(bindings, k)
	switch binding.action {
	case "next":
		return tui.Key{Kind: tui.KeyDown}
	case "previous":
		return tui.Key{Kind: tui.KeyUp}
	default:
		return k
	}
}
