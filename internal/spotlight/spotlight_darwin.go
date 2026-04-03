package spotlight

/*
#cgo CFLAGS: -x objective-c
#cgo LDFLAGS: -framework AppKit -framework Carbon

int registerGlobalHotkey(void);
void showSpotlightPanel(int width, int height);
void dismissSpotlightPanel(void);
*/
import "C"

import (
	"fmt"
	"sync"
)

var (
	callbackMu sync.Mutex
	callbackFn func()
	submitFn   func(text string)
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
func goSpotlightSubmit(ctext *C.char) {
	text := C.GoString(ctext)
	callbackMu.Lock()
	fn := submitFn
	callbackMu.Unlock()
	if fn != nil {
		fn(text)
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

// OnSubmit sets the callback invoked when user submits text in the spotlight panel.
func OnSubmit(fn func(text string)) {
	callbackMu.Lock()
	submitFn = fn
	callbackMu.Unlock()
}

// ShowPanel shows the spotlight overlay panel on the current screen.
func ShowPanel(width, height int) {
	C.showSpotlightPanel(C.int(width), C.int(height))
}

// DismissPanel hides the spotlight panel and returns focus.
func DismissPanel() {
	C.dismissSpotlightPanel()
}
