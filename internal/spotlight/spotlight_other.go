//go:build !darwin

package spotlight

import "fmt"

func Supported() bool { return false }

func Register(_ func()) error {
	return fmt.Errorf("global hotkey not supported on this platform")
}

func OnSubmit(_ func(string, string)) {}
func ShowPanel(_ string)              {}
func DismissPanel()                   {}
