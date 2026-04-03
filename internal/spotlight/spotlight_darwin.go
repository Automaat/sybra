package spotlight

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework AppKit -framework Carbon -framework WebKit
#include <stdlib.h>

int registerGlobalHotkey(void);
void showSpotlightPanel(const char *projectsJSON);
void dismissSpotlightPanel(void);
void resizeSpotlightPanel(int height);
*/
import "C"

import (
	"fmt"
	"sync"
	"unsafe"
)

var (
	callbackMu sync.Mutex
	callbackFn func()
	submitFn   func(title, projectID string)
)

//export goHotkeyCallback
func goHotkeyCallback() {
	callbackMu.Lock()
	fn := callbackFn
	callbackMu.Unlock()
	if fn != nil {
		fn()
	}
}

//export goSpotlightSubmit
func goSpotlightSubmit(ctitle, cprojectID *C.char) {
	title := C.GoString(ctitle)
	projectID := C.GoString(cprojectID)
	callbackMu.Lock()
	fn := submitFn
	callbackMu.Unlock()
	if fn != nil {
		fn(title, projectID)
	}
}

// Register sets up Ctrl+Space as a global hotkey.
func Register(callback func()) error {
	callbackMu.Lock()
	callbackFn = callback
	callbackMu.Unlock()

	status := C.registerGlobalHotkey()
	if status != 0 {
		return fmt.Errorf("RegisterEventHotKey failed: status %d", status)
	}
	return nil
}

// OnSubmit sets the callback invoked when user submits in the spotlight panel.
func OnSubmit(fn func(title, projectID string)) {
	callbackMu.Lock()
	submitFn = fn
	callbackMu.Unlock()
}

// ShowPanel shows the spotlight panel with the given projects JSON array.
func ShowPanel(projectsJSON string) {
	cstr := C.CString(projectsJSON)
	defer C.free(unsafe.Pointer(cstr))
	C.showSpotlightPanel(cstr)
}

// DismissPanel hides the spotlight panel.
func DismissPanel() {
	C.dismissSpotlightPanel()
}
