package notification

// SendDesktop posts an OS-native desktop notification directly, bypassing the
// Emitter buffer, the in-app event path, and the user's desktop toggle. For
// critical conditions (e.g. a stalled UI) that must reach the user even when
// in-app delivery is broken. No-op on non-darwin.
func SendDesktop(title, message string) error {
	return sendDesktopNotification(title, message)
}
