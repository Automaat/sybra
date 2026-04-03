//go:build !darwin

package spotlight

import "fmt"

func Register(_ func()) error {
	return fmt.Errorf("global hotkey not supported on this platform")
}

func OnSubmit(_ func(string)) {}
func ShowPanel(_, _ int)      {}
func DismissPanel()           {}
