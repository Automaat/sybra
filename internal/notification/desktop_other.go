//go:build !darwin

package notification

func sendDesktopNotification(_, _ string) error { return nil }
